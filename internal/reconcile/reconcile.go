package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0x464e/immich-family-bridge/internal/config"
	"github.com/0x464e/immich-family-bridge/internal/domain"
	"github.com/0x464e/immich-family-bridge/internal/filesystem"
	"github.com/0x464e/immich-family-bridge/internal/immich"
	"github.com/0x464e/immich-family-bridge/internal/store"
)

type Reconciler struct {
	C                      config.Config
	DB                     *store.Store
	API                    immich.Client
	FS                     filesystem.Linker
	Log                    *slog.Logger
	mu                     sync.Mutex
	batchCursor            map[string]string
	prepareRemaining       map[string]int
	lookupRemaining        map[string]int
	attemptedPrepare       map[string]bool
	attemptedLookup        map[string]bool
	auditFailed            map[string]bool
	cycleTogetherID        string
	cycleTogetherAssets    map[string]map[string]bool
	stackCache             map[string]domain.Stack
	observedAssetIDs       map[string]map[string]string
	lastScanAt             map[string]time.Time
	scanInterval           time.Duration
	scanLease              time.Duration
	sidecarDiscoveryNeeded bool
	lastSidecarDiscoveryAt time.Time
	now                    func() time.Time
}

var ErrDryRunMode = errors.New("dry-run mode enabled; proposed actions are reported in the service logs")
var ErrPostwork = errors.New("reconciliation postwork incomplete")
var errPendingImport = errors.New("waiting for Immich library import")

const (
	prepareBatchSize = 500
	lookupBatchSize  = 500
	auditBatchSize   = 50
	stackBatchSize   = 50
	albumBatchSize   = 100
	removalBatchSize = 100
)

func New(c config.Config, db *store.Store, api immich.Client, log *slog.Logger) *Reconciler {
	return &Reconciler{
		C: c, DB: db, API: api,
		FS:  filesystem.Linker{SourceRoot: c.SourceRoot, BridgeRoot: c.BridgeRoot, ReadOnly: c.DryRun},
		Log: log, batchCursor: map[string]string{}, lastScanAt: map[string]time.Time{},
		scanInterval: 90 * time.Second, scanLease: 10 * time.Minute, now: time.Now,
	}
}

// requestLibraryScan makes scan requests single-flight across bridge
// restarts and processes. Immich updates refreshedAt when the crawl finishes;
// until that changes, the persisted lease prevents another full scan.
func (r *Reconciler) requestLibraryScan(ctx context.Context, member domain.Member) (bool, error) {
	now := r.now()
	if last := r.lastScanAt[member.ID]; !last.IsZero() && now.Sub(last) < r.scanInterval {
		return false, nil
	}
	baseline := ""
	if library, err := r.API.GetLibrary(ctx, member); err == nil && library.RefreshedAt != nil {
		baseline = library.RefreshedAt.UTC().Format(time.RFC3339Nano)
	}
	lease := r.scanLease
	// A zero scan interval is used by focused tests to request every retry
	// immediately; preserve that behavior without weakening production leases.
	if r.scanInterval == 0 {
		lease = 0
	}
	claimed, err := r.DB.ClaimLibraryScan(member.ID, now, baseline, lease)
	if err != nil || !claimed {
		return false, err
	}
	if err := r.API.ScanLibrary(ctx, member); err != nil {
		_ = r.DB.ReleaseLibraryScan(member.ID, now)
		return false, err
	}
	r.lastScanAt[member.ID] = now
	return true, nil
}
func (r *Reconciler) member(id string) (domain.Member, bool) {
	for _, m := range r.C.Members {
		if m.ID == id {
			return m, true
		}
	}
	return domain.Member{}, false
}

// EnsureTogether keeps one logical catch-all album and adopts uniquely named
// member albums when they already exist. Immich writes remain in Run.
func (r *Reconciler) EnsureTogether(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := r.C.TogetherAlbumName
	if name == "" {
		name = "Together"
	}
	albums, err := r.DB.Albums()
	if err != nil {
		return "", err
	}
	var selected *domain.LogicalAlbum
	for i := range albums {
		if albums[i].SystemKey == "together" {
			selected = &albums[i]
			break
		}
	}
	if selected == nil {
		for i := range albums {
			if albums[i].Name != name {
				continue
			}
			if selected != nil {
				return "", fmt.Errorf("multiple registered albums named %q; cannot choose a Together album", name)
			}
			selected = &albums[i]
		}
	}
	replicas := map[string]string{}
	if selected != nil {
		replicas, err = r.DB.AlbumReplicas(selected.ID)
		if err != nil {
			return "", err
		}
	}
	found := map[string]string{}
	description := ""
	for _, m := range r.C.Members {
		if replicas[m.ID] != "" {
			continue
		}
		album, exists, err := r.findOwnedAlbumByName(ctx, m, name)
		if err != nil {
			return "", err
		}
		if !exists {
			continue
		}
		mapped, known, err := r.DB.AlbumByReplica(m.ID, album.ID)
		if err != nil {
			return "", err
		}
		if known && (selected == nil || mapped != selected.ID) {
			return "", fmt.Errorf("%s album %s is already registered to another logical album", m.ID, album.ID)
		}
		found[m.ID] = album.ID
		if description == "" {
			description = album.Description
		}
	}
	if selected == nil {
		album := domain.LogicalAlbum{ID: store.ID(), Name: name, Description: description, SystemKey: "together"}
		if err := r.DB.AddAlbum(r.C.FamilyID, album, found); err != nil {
			return "", err
		}
		return album.ID, nil
	}
	if selected.SystemKey == "" {
		if err := r.DB.SetAlbumSystemKey(selected.ID, "together"); err != nil {
			return "", err
		}
	}
	if selected.Name != name {
		if err := r.DB.UpdateAlbum(selected.ID, name, selected.Description, selected.CoverID); err != nil {
			return "", err
		}
	}
	for memberID, albumID := range found {
		if err := r.DB.SetAlbumReplica(selected.ID, memberID, albumID); err != nil {
			return "", err
		}
	}
	return selected.ID, nil
}

func (r *Reconciler) findOwnedAlbumByName(ctx context.Context, m domain.Member, name string) (domain.Album, bool, error) {
	albums, err := r.API.ListAlbums(ctx, m)
	if err != nil {
		return domain.Album{}, false, fmt.Errorf("list albums for %s: %w", m.ID, err)
	}
	var match domain.Album
	for _, album := range albums {
		if album.Name != name {
			continue
		}
		if match.ID != "" {
			return domain.Album{}, false, fmt.Errorf("member %s has multiple albums named %q", m.ID, name)
		}
		match = album
	}
	if match.ID == "" {
		return domain.Album{}, false, nil
	}
	match, err = r.API.GetAlbum(ctx, m, match.ID)
	if err != nil {
		return domain.Album{}, false, err
	}
	if match.OwnerID != m.UserID || match.Name != name {
		return domain.Album{}, false, fmt.Errorf("member %s album ownership or name changed during lookup", m.ID)
	}
	return match, true, nil
}

func (r *Reconciler) Register(ctx context.Context, memberID, albumID string) (string, error) {
	return r.RegisterWithReplicas(ctx, memberID, albumID, nil)
}

func (r *Reconciler) RegisterWithReplicas(ctx context.Context, memberID, albumID string, existing map[string]string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.member(memberID)
	if !ok {
		return "", errors.New("unknown member")
	}
	if id, found, err := r.DB.AlbumByReplica(memberID, albumID); err != nil || found {
		if err != nil {
			return id, err
		}
		albums, e := r.DB.Albums()
		if e != nil {
			return id, e
		}
		for _, a := range albums {
			if a.ID == id {
				if r.C.DryRun {
					return id, nil
				}
				return id, r.ensureAlbumReplicas(ctx, a)
			}
		}
		return id, errors.New("album mapping missing")
	}
	a, err := r.API.GetAlbum(ctx, m, albumID)
	if err != nil {
		return "", err
	}
	if a.OwnerID != m.UserID {
		return "", errors.New("album is not owned by member")
	}
	id := store.ID()
	logical := domain.LogicalAlbum{ID: id, Name: a.Name, Description: a.Description}
	replicas := map[string]string{memberID: albumID}
	for otherID, otherAlbumID := range existing {
		if otherID == memberID {
			if otherAlbumID != albumID {
				return "", errors.New("source album ID conflicts with replicas")
			}
			continue
		}
		other, ok := r.member(otherID)
		if !ok || otherAlbumID == "" {
			return "", errors.New("invalid existing album replica")
		}
		if _, found, e := r.DB.AlbumByReplica(otherID, otherAlbumID); e != nil {
			return "", e
		} else if found {
			return "", errors.New("existing album already registered")
		}
		remote, e := r.API.GetAlbum(ctx, other, otherAlbumID)
		if e != nil {
			return "", e
		}
		if remote.OwnerID != other.UserID {
			return "", errors.New("existing album owner mismatch")
		}
		replicas[otherID] = otherAlbumID
	}
	if err := r.DB.AddAlbum(r.C.FamilyID, logical, replicas); err != nil {
		return "", err
	}
	if a.CoverID != "" {
		asset, e := r.API.GetAsset(ctx, m, a.CoverID)
		if e == nil && asset.OwnerID == m.UserID {
			if asset, e = r.mappedAsset(asset); e == nil {
				if lid, e := r.DB.EnsureOrigin(r.C.FamilyID, m.ID, asset); e == nil {
					_ = r.DB.SetCover(id, lid)
				}
			}
		}
	}
	if !r.C.DryRun {
		if err := r.ensureAlbumReplicas(ctx, logical); err != nil {
			return id, err
		}
	}
	return id, nil
}

func (r *Reconciler) UpdateAlbum(id, name, description, cover string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return errors.New("album name required")
	}
	if cover != "" {
		if _, err := r.DB.LogicalAsset(cover); err != nil {
			return errors.New("unknown logical cover asset")
		}
	}
	return r.DB.UpdateAlbum(id, name, description, cover)
}

func (r *Reconciler) ensureAlbumReplicas(ctx context.Context, a domain.LogicalAlbum) error {
	reps, err := r.DB.AlbumReplicas(a.ID)
	if err != nil {
		return err
	}
	for _, m := range r.C.Members {
		if reps[m.ID] != "" {
			continue
		}
		if a.SystemKey == "together" {
			existing, found, err := r.findOwnedAlbumByName(ctx, m, a.Name)
			if err != nil {
				return err
			}
			if found {
				if err := r.DB.SetAlbumReplica(a.ID, m.ID, existing.ID); err != nil {
					return err
				}
				continue
			}
		}
		made, err := r.API.CreateAlbum(ctx, m, a.ID, a.Name, a.Description)
		if err != nil {
			return fmt.Errorf("create album for %s: %w", m.ID, err)
		}
		if made.OwnerID != "" && made.OwnerID != m.UserID {
			return errors.New("created album owner mismatch")
		}
		if err := r.DB.SetAlbumReplica(a.ID, m.ID, made.ID); err != nil {
			return err
		}
	}
	return nil
}

type observed struct {
	member           domain.Member
	albumID          string
	assets           map[string]bool
	prior            map[string]bool
	persisted        map[string]bool
	name             string
	description      string
	fromSnapshot     bool
	coverID          string
	previousCoverID  string
	coverWasObserved bool
}

func (r *Reconciler) Run(ctx context.Context) error {
	if r.C.DryRun {
		return ErrDryRunMode
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	started := r.now()
	if err := r.startCycle(prepareBatchSize, lookupBatchSize); err != nil {
		return err
	}
	albums, err := r.DB.Albums()
	if err != nil {
		return err
	}
	r.observedAssetIDs = map[string]map[string]string{}
	for _, member := range r.C.Members {
		mapped, err := r.DB.ReplicaAssetIDs(member.ID)
		if err != nil {
			return err
		}
		r.observedAssetIDs[member.ID] = mapped
	}
	// Observe every album before changing memberships or deleting replicas. A
	// failed read must never turn an incomplete view into an unsharing decision.
	plans := []*albumPlan{}
	for _, a := range albums {
		if err := r.ensureAlbumReplicas(ctx, a); err != nil {
			return err
		}
		plan, err := r.observeAlbum(ctx, a, false)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
	}
	resolveTogether(plans)
	if err := r.persistPlans(plans); err != nil {
		return err
	}
	if err := r.discoverSourceStacks(ctx); err != nil {
		return err
	}
	if err := r.auditReadyReplicas(ctx, plans, auditBatchSize); err != nil {
		return err
	}
	return r.applyPlans(ctx, plans, started, "discovery")
}

// Work continues only decisions already persisted by a successful full
// observation. It makes no membership decisions and performs no album search.
// This lets a large import advance without repeatedly crawling every album.
func (r *Reconciler) Work(ctx context.Context) error {
	if r.C.DryRun {
		return ErrDryRunMode
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	started := r.now()
	if err := r.startCycle(100, 50); err != nil {
		return err
	}
	albums, err := r.DB.Albums()
	if err != nil {
		return err
	}
	plans := make([]*albumPlan, 0, len(albums))
	for _, a := range albums {
		if !a.Initialized {
			continue
		}
		desired, err := r.DB.Memberships(a.ID)
		if err != nil {
			return err
		}
		reps, err := r.DB.AlbumReplicas(a.ID)
		if err != nil {
			return err
		}
		plan := &albumPlan{album: a, desired: desired}
		for _, member := range r.C.Members {
			albumID := reps[member.ID]
			if albumID == "" {
				return fmt.Errorf("missing album replica for %s", member.ID)
			}
			assets, err := r.DB.Observations(a.ID, member.ID)
			if err != nil {
				return err
			}
			cover, _, err := r.DB.AlbumCoverObservation(a.ID, member.ID)
			if err != nil {
				return err
			}
			persisted := make(map[string]bool, len(assets))
			for id := range assets {
				persisted[id] = true
			}
			plan.observations = append(plan.observations, observed{member: member, albumID: albumID, assets: assets, persisted: persisted, name: a.Name, description: a.Description, coverID: cover, fromSnapshot: true})
		}
		plans = append(plans, plan)
	}
	sort.SliceStable(plans, func(i, j int) bool {
		return plans[i].album.SystemKey == "together" && plans[j].album.SystemKey != "together"
	})
	if err := r.auditReadyReplicas(ctx, plans, auditBatchSize/2); err != nil {
		return err
	}
	return r.applyPlans(ctx, plans, started, "work")
}

func (r *Reconciler) startCycle(prepareLimit, lookupLimit int) error {
	cursors, err := r.DB.WorkCursors()
	if err != nil {
		return err
	}
	r.batchCursor = cursors
	r.stackCache = nil
	r.observedAssetIDs = nil
	r.prepareRemaining = map[string]int{}
	r.lookupRemaining = map[string]int{}
	r.attemptedPrepare = map[string]bool{}
	r.attemptedLookup = map[string]bool{}
	r.auditFailed = map[string]bool{}
	for _, member := range r.C.Members {
		r.prepareRemaining[member.ID] = prepareLimit
		r.lookupRemaining[member.ID] = lookupLimit
	}
	return nil
}

func (r *Reconciler) applyPlans(ctx context.Context, plans []*albumPlan, started time.Time, mode string) error {
	r.cycleTogetherID = ""
	r.cycleTogetherAssets = map[string]map[string]bool{}
	for _, plan := range plans {
		if plan.album.SystemKey == "together" {
			r.cycleTogetherID = plan.album.ID
			break
		}
	}
	var errs []error
	for _, plan := range plans {
		if err := r.applyAlbum(ctx, plan); err != nil {
			return fmt.Errorf("album %s: %w", plan.album.ID, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e := r.reconcileStacks(ctx); e != nil {
		errs = append(errs, e)
	}
	// Cleanup runs before MarkUnused so a newly removed sharing reference gets
	// one full polling interval to be re-added before destructive work begins.
	if r.C.RemoveUnshared {
		if e := r.cleanupUnusedReplicas(ctx); e != nil {
			errs = append(errs, e)
		}
	}
	if e := r.DB.MarkUnused(); e != nil {
		errs = append(errs, e)
	}
	if r.sidecarDiscoveryNeeded {
		now := r.now()
		if r.lastSidecarDiscoveryAt.IsZero() || now.Sub(r.lastSidecarDiscoveryAt) >= r.scanInterval {
			r.lastSidecarDiscoveryAt = now
			if e := r.API.DiscoverSidecars(ctx); e != nil {
				r.Log.Warn("Immich sidecar discovery request failed", "error", e)
			} else {
				r.sidecarDiscoveryNeeded = false
				r.Log.Info("Immich sidecar discovery requested")
			}
		}
	}
	if e := r.DB.SaveWorkCursors(r.batchCursor); e != nil {
		errs = append(errs, e)
	}
	r.Log.Info("reconciliation cycle complete", "mode", mode, "duration", r.now().Sub(started), "albums", len(plans), "errors", len(errs))
	if len(errs) != 0 {
		return fmt.Errorf("%w: %w", ErrPostwork, errors.Join(errs...))
	}
	return nil
}

// Audit each ready logical asset at most once per member and cycle, even when
// it belongs to many albums. A rotating cursor makes the slow correctness
// sweep bounded without permanently starving the tail of a large library.
func (r *Reconciler) auditReadyReplicas(ctx context.Context, plans []*albumPlan, limit int) error {
	desired := map[string]bool{}
	for _, plan := range plans {
		for id := range plan.desired {
			desired[id] = true
		}
	}
	for _, member := range r.C.Members {
		reps, err := r.DB.ReplicasForMember(member.ID)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(desired))
		for id := range desired {
			if rep, ok := reps[id]; ok && rep.State == "ready" && rep.AssetID != "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range r.batchAfter("audit:"+member.ID, ids, limit) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := r.ensureReplica(ctx, id, member); err != nil {
				if canceled := ctx.Err(); canceled != nil {
					return canceled
				}
				if errors.Is(err, errPendingImport) {
					continue
				}
				r.auditFailed[member.ID+"\x00"+id] = true
				r.Log.Warn("asset replica audit failed", "logical_asset_id", id, "member_id", member.ID, "error", err)
			}
		}
	}
	return nil
}

// rememberSourceStack records stack associations observed while auditing a
// source asset. Media replication remains independent from this relationship;
// the later stack pass creates it only after every member is ready.
func (r *Reconciler) rememberSourceStack(memberID string, asset domain.Asset) error {
	if asset.StackID == "" || asset.StackPrimaryID == "" {
		return nil
	}
	return r.DB.UpsertSourceStack(domain.SourceStack{SourceMember: memberID, SourceStack: asset.StackID, PrimaryAsset: asset.StackPrimaryID})
}

// Album search does not include stack metadata in Immich 3.2. A single stack
// listing per member is therefore needed to discover retroactive changes
// without probing every source asset individually. Only stacks containing a
// mapped origin are considered; recipient stacks must never become sources.
func (r *Reconciler) discoverSourceStacks(ctx context.Context) error {
	r.stackCache = map[string]domain.Stack{}
	for _, member := range r.C.Members {
		if err := ctx.Err(); err != nil {
			return err
		}
		stacks, err := r.API.ListStacks(ctx, member)
		if err != nil {
			r.Log.Warn("source stack listing failed", "member_id", member.ID, "error", err)
			continue
		}
		replicas, err := r.DB.ReplicasForMember(member.ID)
		if err != nil {
			return err
		}
		origins := map[string]bool{}
		for _, replica := range replicas {
			if replica.Role == "origin" && replica.AssetID != "" {
				origins[replica.AssetID] = true
			}
		}
		for _, stack := range stacks {
			if stack.ID == "" || stack.PrimaryAssetID == "" || len(stack.Assets) < 2 {
				continue
			}
			owned := false
			valid := true
			for _, asset := range stack.Assets {
				if asset.OwnerID != member.UserID || asset.LibraryID == member.LibraryID {
					valid = false
					break
				}
				owned = owned || origins[asset.ID]
			}
			if !valid || !owned {
				continue
			}
			if err := r.DB.UpsertSourceStack(domain.SourceStack{SourceMember: member.ID, SourceStack: stack.ID, PrimaryAsset: stack.PrimaryAssetID}); err != nil {
				return err
			}
			r.stackCache[member.ID+"\x00"+stack.ID] = stack
		}
	}
	return nil
}

func (r *Reconciler) reconcileStacks(ctx context.Context) error {
	sources, err := r.DB.SourceStacks()
	if err != nil {
		return err
	}
	byKey := make(map[string]domain.SourceStack, len(sources))
	keys := make([]string, 0, len(sources))
	for _, source := range sources {
		key := source.SourceMember + "\x00" + source.SourceStack
		keys = append(keys, key)
		byKey[key] = source
	}
	// SourceStacks is already ordered by these two columns.
	selected := r.batchAfter("stack", keys, stackBatchSize)
	var errs []error
	for _, key := range selected {
		source := byKey[key]
		if err := ctx.Err(); err != nil {
			return err
		}
		member, ok := r.member(source.SourceMember)
		if !ok {
			errs = append(errs, fmt.Errorf("stack %s source member missing", source.SourceStack))
			continue
		}
		stack, cached := r.stackCache[key]
		var err error
		if !cached {
			stack, err = r.API.GetStack(ctx, member, source.SourceStack)
		}
		if err != nil {
			// Immich 3.2 returns HTTP 400 for a deleted stack. Do not treat an
			// arbitrary stack-read failure as deletion: independently confirm
			// that the recorded source primary no longer belongs to this stack.
			gone, confirmErr := r.sourceStackGone(ctx, member, source)
			if confirmErr == nil && gone {
				if e := r.removeMirroredStack(ctx, source); e != nil {
					errs = append(errs, e)
				}
				continue
			}
			if confirmErr != nil {
				errs = append(errs, fmt.Errorf("confirm source stack %s: %w", source.SourceStack, confirmErr))
				continue
			}
			errs = append(errs, fmt.Errorf("read source stack %s: %w", source.SourceStack, err))
			continue
		}
		if stack.OwnerID != "" && stack.OwnerID != member.UserID {
			errs = append(errs, fmt.Errorf("source stack %s owner mismatch", source.SourceStack))
			continue
		}
		if len(stack.Assets) < 2 || stack.PrimaryAssetID == "" {
			errs = append(errs, fmt.Errorf("source stack %s has invalid membership", source.SourceStack))
			continue
		}
		if err := r.DB.UpsertSourceStack(domain.SourceStack{SourceMember: source.SourceMember, SourceStack: source.SourceStack, PrimaryAsset: stack.PrimaryAssetID}); err != nil {
			return err
		}
		for _, target := range r.C.Members {
			if target.ID == source.SourceMember {
				continue
			}
			if e := r.reconcileStackReplica(ctx, source, stack, target); e != nil {
				errs = append(errs, e)
			}
		}
	}
	if len(sources) > stackBatchSize {
		r.Log.Info("stack sweep progress", "processed", len(selected), "known", len(sources))
	}
	return errors.Join(errs...)
}

func (r *Reconciler) sourceStackGone(ctx context.Context, member domain.Member, source domain.SourceStack) (bool, error) {
	asset, err := r.API.GetAsset(ctx, member, source.PrimaryAsset)
	if err != nil {
		return false, err
	}
	return asset.StackID != source.SourceStack, nil
}

func (r *Reconciler) reconcileStackReplica(ctx context.Context, source domain.SourceStack, stack domain.Stack, target domain.Member) error {
	assetIDs, complete, err := r.stackRecipientAssets(source.SourceMember, stack, target.ID)
	if err != nil {
		return err
	}
	current, found, err := r.DB.StackReplica(source.SourceMember, source.SourceStack, target.ID)
	if err != nil {
		return err
	}
	if !complete {
		if found && current.StackID != "" {
			if err := r.deleteRecipientStack(ctx, target, current); err != nil {
				return fmt.Errorf("remove incomplete recipient stack %s for %s: %w", current.StackID, target.ID, err)
			}
		}
		return r.DB.UpsertStackReplica(domain.StackReplica{SourceMember: source.SourceMember, SourceStack: source.SourceStack, MemberID: target.ID, State: "pending", Error: "waiting for every active stack member replica"})
	}
	signature := strings.Join(assetIDs, "\x00")
	if found && current.State == "ready" && current.Signature == signature && current.StackID != "" {
		return nil
	}
	if found && current.StackID != "" {
		if err := r.deleteRecipientStack(ctx, target, current); err != nil {
			return fmt.Errorf("replace recipient stack %s for %s: %w", current.StackID, target.ID, err)
		}
	}
	made, err := r.API.CreateStack(ctx, target, assetIDs)
	if err != nil {
		_ = r.DB.UpsertStackReplica(domain.StackReplica{SourceMember: source.SourceMember, SourceStack: source.SourceStack, MemberID: target.ID, Signature: signature, State: "error", Error: err.Error()})
		return fmt.Errorf("create recipient stack for %s: %w", target.ID, err)
	}
	if made.ID == "" || made.PrimaryAssetID != assetIDs[0] {
		return fmt.Errorf("recipient stack response did not preserve primary asset for %s", target.ID)
	}
	if err := r.DB.UpsertStackReplica(domain.StackReplica{SourceMember: source.SourceMember, SourceStack: source.SourceStack, MemberID: target.ID, StackID: made.ID, Signature: signature, State: "ready"}); err != nil {
		return err
	}
	r.Log.Info("recipient stack ready", "source_member_id", source.SourceMember, "source_stack_id", source.SourceStack, "member_id", target.ID, "immich_stack_id", made.ID)
	return nil
}

// deleteRecipientStack is restart-safe on Immich 3.2, which also returns
// HTTP 400 when a just-deleted stack is queried or deleted again. A missing
// remote stack is accepted only after its recorded primary recipient asset no
// longer points at that stack; permission and unrelated API failures remain
// errors.
func (r *Reconciler) deleteRecipientStack(ctx context.Context, member domain.Member, replica domain.StackReplica) error {
	err := r.API.DeleteStack(ctx, member, replica.StackID)
	if err == nil || errors.Is(err, immich.ErrNotFound) {
		return nil
	}
	primary := strings.SplitN(replica.Signature, "\x00", 2)[0]
	if primary == "" {
		return err
	}
	asset, inspectErr := r.API.GetAsset(ctx, member, primary)
	if inspectErr == nil && asset.StackID != replica.StackID {
		return nil
	}
	return err
}

// stackRecipientAssets returns a target stack in source-primary order. A
// stack is mirrored all-or-nothing: creating a partial stack changes the
// meaning of the source grouping and can hide an unrelated recipient asset.
func (r *Reconciler) stackRecipientAssets(sourceMember string, stack domain.Stack, targetMember string) ([]string, bool, error) {
	bySourceID := make(map[string]domain.Asset, len(stack.Assets))
	for _, asset := range stack.Assets {
		bySourceID[asset.ID] = asset
	}
	primary, ok := bySourceID[stack.PrimaryAssetID]
	if !ok {
		return nil, false, errors.New("source stack omitted primary asset")
	}
	ordered := []domain.Asset{primary}
	others := make([]domain.Asset, 0, len(stack.Assets)-1)
	for _, asset := range stack.Assets {
		if asset.ID != primary.ID {
			others = append(others, asset)
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].ID < others[j].ID })
	ordered = append(ordered, others...)
	result := make([]string, 0, len(ordered))
	for _, asset := range ordered {
		logicalID, known, err := r.DB.FindReplicaAsset(sourceMember, asset.ID)
		if err != nil {
			return nil, false, err
		}
		if !known {
			return nil, false, nil
		}
		logical, err := r.DB.LogicalAsset(logicalID)
		if err != nil {
			return nil, false, err
		}
		if logical.OriginMember != sourceMember || logical.OriginAsset != asset.ID {
			return nil, false, nil
		}
		count, err := r.DB.SourceCount(logicalID)
		if err != nil {
			return nil, false, err
		}
		if count == 0 {
			return nil, false, nil
		}
		replica, ready, err := r.DB.Replica(logicalID, targetMember)
		if err != nil {
			return nil, false, err
		}
		if !ready || replica.State != "ready" || replica.AssetID == "" {
			return nil, false, nil
		}
		result = append(result, replica.AssetID)
	}
	return result, true, nil
}

func (r *Reconciler) removeMirroredStack(ctx context.Context, source domain.SourceStack) error {
	for _, member := range r.C.Members {
		if member.ID == source.SourceMember {
			continue
		}
		replica, found, err := r.DB.StackReplica(source.SourceMember, source.SourceStack, member.ID)
		if err != nil {
			return err
		}
		if found && replica.StackID != "" {
			if err := r.deleteRecipientStack(ctx, member, replica); err != nil {
				return fmt.Errorf("remove recipient stack %s for %s: %w", replica.StackID, member.ID, err)
			}
		}
	}
	return r.DB.DeleteSourceStack(source.SourceMember, source.SourceStack)
}

func (r *Reconciler) cleanupUnusedReplicas(ctx context.Context) error {
	replicas, err := r.DB.RemovalReplicas(removalBatchSize)
	if err != nil {
		return err
	}
	var errs []error
	for _, replica := range replicas {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := r.DB.SourceCount(replica.LogicalID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if count != 0 {
			continue
		}
		member, ok := r.member(replica.MemberID)
		if !ok {
			errs = append(errs, fmt.Errorf("cleanup replica %s/%s: member missing", replica.LogicalID, replica.MemberID))
			continue
		}
		if err := r.cleanupReplica(ctx, member, replica); err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			replica.Error = err.Error()
			_ = r.DB.UpsertReplica(replica)
			r.Log.Warn("recipient replica cleanup pending", "logical_asset_id", replica.LogicalID, "member_id", replica.MemberID, "error", err)
			errs = append(errs, fmt.Errorf("cleanup replica %s/%s: %w", replica.LogicalID, replica.MemberID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) cleanupReplica(ctx context.Context, member domain.Member, replica domain.Replica) error {
	remotePath, err := r.remoteRecipientPath(replica.Path)
	if err != nil && replica.Path != "" {
		return err
	}
	if replica.AssetID == "" && remotePath != "" {
		found, err := r.API.FindByPath(ctx, member, remotePath)
		if err != nil {
			return err
		}
		if len(found) > 1 {
			return fmt.Errorf("expected at most one imported asset at %s, got %d", remotePath, len(found))
		}
		if len(found) == 1 {
			if found[0].OwnerID != member.UserID || found[0].LibraryID != member.LibraryID || found[0].OriginalPath != remotePath {
				return errors.New("recipient asset identity mismatch during cleanup")
			}
			replica.AssetID = found[0].ID
		} else {
			// A scan already queued while the asset was shared may still import
			// this path. Keep the hardlinks and mapping until that race settles;
			// once imported, the normal verified deletion path removes it.
			if _, err := r.requestLibraryScan(ctx, member); err != nil {
				return err
			}
			return nil
		}
	}
	if replica.AssetID != "" {
		asset, err := r.API.GetAsset(ctx, member, replica.AssetID)
		if err == nil {
			if asset.OwnerID != member.UserID || asset.LibraryID != member.LibraryID || asset.OriginalPath != remotePath {
				return errors.New("recipient asset identity mismatch during cleanup")
			}
			if err := r.API.DeleteAssets(ctx, member, []string{replica.AssetID}); err != nil {
				return err
			}
			replica.State = "deleting"
			replica.Error = ""
			if err := r.DB.UpsertReplica(replica); err != nil {
				return err
			}
			r.Log.Info("recipient replica deletion requested", "logical_asset_id", replica.LogicalID, "member_id", replica.MemberID, "immich_asset_id", replica.AssetID)
			return nil
		}
		if !errors.Is(err, immich.ErrNotFound) {
			return err
		}
	}
	if err := r.removeReplicaFiles(ctx, replica); err != nil {
		return err
	}
	if err := r.DB.DeleteReplica(replica.LogicalID, replica.MemberID); err != nil {
		return err
	}
	r.Log.Info("recipient replica removed", "logical_asset_id", replica.LogicalID, "member_id", replica.MemberID)
	return nil
}

func (r *Reconciler) remoteRecipientPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	rel, err := filepath.Rel(r.C.BridgeRoot, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("recipient path outside bridge root")
	}
	remote := filepath.Join(r.C.ImmichBridgeRoot, rel)
	if !strings.HasPrefix(remote, r.C.ImmichBridgeRoot+string(filepath.Separator)) {
		return "", errors.New("invalid remote recipient path")
	}
	return remote, nil
}

func (r *Reconciler) removeReplicaFiles(ctx context.Context, replica domain.Replica) error {
	components, err := r.DB.ReplicaFiles(replica.LogicalID, replica.MemberID)
	if err != nil {
		return err
	}
	kinds := make([]string, 0, len(components))
	for kind := range components {
		kinds = append(kinds, kind)
	}
	// Remove companions before the original. Immich may already have removed
	// either path, which Linker.Remove treats as success.
	sort.Slice(kinds, func(i, j int) bool {
		if kinds[i] == "original" {
			return false
		}
		if kinds[j] == "original" {
			return true
		}
		return kinds[i] < kinds[j]
	})
	for _, kind := range kinds {
		component := components[kind]
		if err := r.FS.Remove(component.SourcePath, component.RecipientPath); err != nil {
			return fmt.Errorf("remove %s component: %w", kind, err)
		}
	}
	if len(components) == 0 && replica.Path != "" {
		logical, err := r.DB.LogicalAsset(replica.LogicalID)
		if err != nil {
			return err
		}
		origin, ok := r.member(logical.OriginMember)
		if !ok {
			return errors.New("origin member missing")
		}
		asset, err := r.API.GetAsset(ctx, origin, logical.OriginAsset)
		if err != nil {
			return fmt.Errorf("inspect origin during cleanup: %w", err)
		}
		asset, err = r.mappedAsset(asset)
		if err != nil {
			return err
		}
		if err := r.FS.Remove(asset.OriginalPath, replica.Path); err != nil {
			return fmt.Errorf("remove original component: %w", err)
		}
	}
	return nil
}

func (r *Reconciler) applyAlbum(ctx context.Context, plan *albumPlan) error {
	a, obs, desired := plan.album, plan.observations, plan.desired
	if a.CoverID != "" && !desired[a.CoverID] {
		if err := r.DB.SetCover(a.ID, ""); err != nil {
			return err
		}
		a.CoverID = ""
	}
	for _, o := range obs {
		var togetherAssets map[string]bool
		if a.SystemKey != "together" && r.cycleTogetherID != "" {
			var ok bool
			togetherAssets, ok = r.cycleTogetherAssets[o.member.ID]
			if !ok {
				var err error
				togetherAssets, err = r.DB.Observations(r.cycleTogetherID, o.member.ID)
				if err != nil {
					return err
				}
				r.cycleTogetherAssets[o.member.ID] = togetherAssets
			}
		}
		if err := r.reconcileMemberAssets(ctx, a.ID, o, desired, togetherAssets); err != nil {
			return err
		}
		coverID := ""
		if a.CoverID != "" {
			cover, found, err := r.DB.Replica(a.CoverID, o.member.ID)
			if err != nil {
				return err
			}
			if found && cover.AssetID != "" && cover.State == "ready" {
				coverID = cover.AssetID
			}
		}
		if !o.fromSnapshot {
			if o.name != a.Name || o.description != a.Description || (coverID != "" && coverID != o.coverID) {
				if err := r.API.UpdateAlbum(ctx, o.member, o.albumID, a.Name, a.Description, coverID); err != nil {
					return err
				}
			}
			observedCover := o.coverID
			if coverID != "" {
				observedCover = coverID
			}
			if err := r.DB.SetAlbumCoverObservation(a.ID, o.member.ID, observedCover); err != nil {
				return err
			}
		}
		mappedAssets, err := r.DB.ReplicaAssetIDs(o.member.ID)
		if err != nil {
			return err
		}
		for assetID := range o.assets {
			lid, known := mappedAssets[assetID]
			if known && !desired[lid] {
				if err := r.API.RemoveAssets(ctx, o.member, o.albumID, []string{assetID}); err != nil {
					return err
				}
				delete(o.assets, assetID)
			}
		}
		if err := r.DB.UpdateObservations(a.ID, o.member.ID, o.persisted, o.assets); err != nil {
			return err
		}
	}
	return nil
}

// batchAfter rotates through a sorted worklist so that pending imports and
// ready audits keep making progress even when there are more than one cycle's
// worth of assets. The cursor only affects scheduling; replica state is in SQLite.
func (r *Reconciler) batchAfter(key string, ids []string, limit int) []string {
	if len(ids) == 0 || limit <= 0 {
		return nil
	}
	start := sort.Search(len(ids), func(i int) bool { return ids[i] > r.batchCursor[key] })
	if start == len(ids) {
		start = 0
	}
	count := min(len(ids), limit)
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, ids[(start+i)%len(ids)])
	}
	r.batchCursor[key] = out[len(out)-1]
	return out
}

func (r *Reconciler) reconcileMemberAssets(ctx context.Context, albumID string, o observed, desired map[string]bool, togetherAssets map[string]bool) error {
	replicas, err := r.DB.ReplicasForMember(o.member.ID)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(desired))
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	ready := make(map[string]domain.Replica, len(ids))
	needsPrepare := []string{}
	pending := map[string]bool{}
	unsupported := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		rep, found := replicas[id]
		switch {
		case r.auditFailed[o.member.ID+"\x00"+id]:
			continue
		case found && rep.State == "ready" && rep.AssetID != "":
			ready[id] = rep
		case found && rep.State == "pending_import" && rep.Path != "":
			pending[id] = true
		case found && rep.State == "unsupported":
			if rep.Error == "coupled or unsupported media type" {
				needsPrepare = append(needsPrepare, id)
			} else {
				unsupported++
			}
		default:
			needsPrepare = append(needsPrepare, id)
		}
	}
	toPrepare := r.batchAfter("prepare:"+o.member.ID, needsPrepare, r.prepareRemaining[o.member.ID])
	for _, id := range toPrepare {
		workKey := o.member.ID + "\x00" + id
		if r.attemptedPrepare[workKey] {
			continue
		}
		r.attemptedPrepare[workKey] = true
		r.prepareRemaining[o.member.ID]--
		if err := ctx.Err(); err != nil {
			return err
		}
		rep, err := r.ensureReplica(ctx, id, o.member)
		replicas[id] = rep
		if errors.Is(err, errPendingImport) {
			pending[id] = true
			continue
		}
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			r.Log.Warn("asset replica pending", "logical_asset_id", id, "member_id", o.member.ID, "error", err)
			continue
		}
		if rep.State == "ready" && rep.AssetID != "" {
			ready[id] = rep
		}
	}
	scanned := false
	if len(pending) > 0 {
		requested, err := r.requestLibraryScan(ctx, o.member)
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			r.Log.Warn("recipient library scan request failed", "member_id", o.member.ID, "error", err)
		} else {
			scanned = requested
		}
	}
	pendingIDs := make([]string, 0, len(pending))
	for id := range pending {
		pendingIDs = append(pendingIDs, id)
	}
	sort.Strings(pendingIDs)
	imported := 0
	for _, id := range r.batchAfter("lookup:"+o.member.ID, pendingIDs, r.lookupRemaining[o.member.ID]) {
		workKey := o.member.ID + "\x00" + id
		if r.attemptedLookup[workKey] {
			continue
		}
		r.attemptedLookup[workKey] = true
		r.lookupRemaining[o.member.ID]--
		if err := ctx.Err(); err != nil {
			return err
		}
		rep, found := replicas[id]
		if !found {
			return fmt.Errorf("pending replica mapping disappeared for %s", id)
		}
		rep, err = r.resolvePendingImport(ctx, rep, o.member)
		replicas[id] = rep
		if errors.Is(err, errPendingImport) {
			continue
		}
		if err != nil {
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			r.Log.Warn("asset import lookup failed", "logical_asset_id", id, "member_id", o.member.ID, "error", err)
			continue
		}
		ready[id] = rep
		imported++
	}
	toAdd := []string{}
	for _, id := range ids {
		if rep, ok := ready[id]; ok && rep.AssetID != "" && !o.assets[rep.AssetID] && (togetherAssets == nil || togetherAssets[rep.AssetID]) {
			toAdd = append(toAdd, rep.AssetID)
		}
	}
	for start := 0; start < len(toAdd); start += albumBatchSize {
		end := min(start+albumBatchSize, len(toAdd))
		if err := r.API.AddAssets(ctx, o.member, o.albumID, toAdd[start:end]); err != nil {
			return fmt.Errorf("add assets to album for %s: %w", o.member.ID, err)
		}
		for _, assetID := range toAdd[start:end] {
			o.assets[assetID] = true
		}
	}
	if len(pending) > 0 || len(needsPrepare) > 0 || len(toAdd) > 0 {
		r.Log.Info("member reconciliation progress", "member_id", o.member.ID,
			"album_id", albumID, "desired", len(ids), "ready", len(ready),
			"pending_import", len(pending)-imported, "waiting_to_prepare", len(needsPrepare)-len(toPrepare),
			"unsupported", unsupported, "imported", imported, "album_added", len(toAdd), "scan_requested", scanned)
	}
	return nil
}

func (r *Reconciler) resolvePendingImport(ctx context.Context, rep domain.Replica, m domain.Member) (domain.Replica, error) {
	rel, err := filepath.Rel(r.C.BridgeRoot, rep.Path)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return rep, errors.New("pending recipient path outside bridge root")
	}
	remotePath := filepath.Join(r.C.ImmichBridgeRoot, rel)
	found, err := r.API.FindByPath(ctx, m, remotePath)
	if err != nil {
		return rep, err
	}
	if len(found) == 0 {
		return rep, errPendingImport
	}
	if len(found) != 1 {
		return rep, fmt.Errorf("expected one imported asset at %s, got %d", remotePath, len(found))
	}
	if found[0].OwnerID != m.UserID || found[0].LibraryID != m.LibraryID || found[0].OriginalPath != remotePath {
		return rep, errors.New("imported asset identity mismatch")
	}
	rep.AssetID = found[0].ID
	rep.State = "ready"
	rep.Error = ""
	if err := r.DB.UpsertReplica(rep); err != nil {
		return rep, err
	}
	r.Log.Info("asset replica ready", "logical_asset_id", rep.LogicalID, "member_id", m.ID, "immich_asset_id", rep.AssetID)
	return rep, nil
}

func (r *Reconciler) ensureReplica(ctx context.Context, logicalID string, m domain.Member) (domain.Replica, error) {
	current, found, err := r.DB.Replica(logicalID, m.ID)
	if err != nil {
		return current, err
	}
	wasReady := found && current.AssetID != "" && current.State == "ready"
	logical, err := r.DB.LogicalAsset(logicalID)
	if err != nil {
		return current, err
	}
	if logical.OriginMember == m.ID {
		if !found || current.Role != "origin" || current.AssetID != logical.OriginAsset {
			return current, errors.New("missing origin replica mapping")
		}
		originAsset, e := r.API.GetAsset(ctx, m, current.AssetID)
		if e != nil {
			if wasReady && !errors.Is(e, immich.ErrNotFound) {
				return current, e
			}
			current.State = "api_error"
			if errors.Is(e, immich.ErrNotFound) {
				current.State = "source_missing"
			}
			current.Error = e.Error()
			_ = r.DB.UpsertReplica(current)
			return current, e
		}
		originAsset, e = r.mappedAsset(originAsset)
		if e != nil || originAsset.OwnerID != m.UserID {
			return current, errors.New("origin asset path or owner changed")
		}
		if e := r.rememberSourceStack(m.ID, originAsset); e != nil {
			return current, e
		}
		path := originAsset.OriginalPath
		if path != current.Path {
			if e := r.rebindOriginPath(logicalID, m.ID, current.Path, path); e != nil {
				return current, fmt.Errorf("origin path changed: %w", e)
			}
			current.Path = path
			r.Log.Info("origin path updated after same-inode move", "logical_asset_id", logicalID, "member_id", m.ID)
		}
		if current.State != "ready" || current.Error != "" {
			current.State, current.Error = "ready", ""
			if e := r.DB.UpsertReplica(current); e != nil {
				return current, e
			}
		}
		if e := r.recordOriginComponents(logicalID, m.ID, originAsset); e != nil {
			return current, e
		}
		return current, nil
	}
	origin, ok := r.member(logical.OriginMember)
	if !ok {
		return current, errors.New("origin member missing")
	}
	asset, err := r.API.GetAsset(ctx, origin, logical.OriginAsset)
	if err != nil {
		if wasReady && !errors.Is(err, immich.ErrNotFound) {
			return current, fmt.Errorf("source asset unavailable: %w", err)
		}
		if !found {
			current = domain.Replica{LogicalID: logicalID, MemberID: m.ID, Role: "external_replica", LibraryID: m.LibraryID}
		}
		current.State = "api_error"
		if errors.Is(err, immich.ErrNotFound) {
			current.State = "source_missing"
		}
		current.Error = err.Error()
		_ = r.DB.UpsertReplica(current)
		return current, fmt.Errorf("source asset unavailable: %w", err)
	}
	if asset.OwnerID != origin.UserID {
		return current, errors.New("source owner changed")
	}
	if err := r.rememberSourceStack(origin.ID, asset); err != nil {
		return current, err
	}
	if !asset.Supported() {
		if !found {
			current = domain.Replica{LogicalID: logicalID, MemberID: m.ID, Role: "external_replica", LibraryID: m.LibraryID}
		}
		current.State = "unsupported"
		current.Error = asset.UnsupportedReason()
		_ = r.DB.UpsertReplica(current)
		return current, errors.New(current.Error)
	}
	asset, err = r.mappedAsset(asset)
	if err != nil {
		return current, err
	}
	path, err := r.FS.Destination(r.C.FamilyID, m.ID, logical.OriginMember, logical.OriginAsset, asset.OriginalPath)
	if err != nil {
		return current, err
	}
	rel, err := filepath.Rel(r.C.BridgeRoot, path)
	if err != nil {
		return current, err
	}
	remotePath := filepath.Join(r.C.ImmichBridgeRoot, rel)
	if !strings.HasPrefix(remotePath, r.C.ImmichBridgeRoot+string(filepath.Separator)) {
		return current, errors.New("invalid remote bridge path")
	}
	if found && current.Path != "" && current.Path != path {
		return current, errors.New("stored recipient path differs from deterministic path")
	}
	if wasReady {
		remote, e := r.API.GetAsset(ctx, m, current.AssetID)
		if e == nil && (remote.OwnerID != m.UserID || remote.LibraryID != m.LibraryID || remote.OriginalPath != remotePath) {
			return current, errors.New("stored recipient asset identity mismatch")
		}
		if e == nil {
			if e := r.ensureRecipientComponents(ctx, logicalID, m, current, asset, path, &remote); e != nil {
				current.Error = e.Error()
				_ = r.DB.UpsertReplica(current)
				return current, e
			}
			if current.Error != "" {
				current.Error = ""
				if e := r.DB.UpsertReplica(current); e != nil {
					return current, e
				}
			}
			return current, nil
		}
		if !errors.Is(e, immich.ErrNotFound) {
			return current, fmt.Errorf("check recipient asset: %w", e)
		}
		r.Log.Warn("recipient Immich asset missing; rediscovering", "logical_asset_id", logicalID, "member_id", m.ID, "error", e)
	}
	current = domain.Replica{LogicalID: logicalID, MemberID: m.ID, Role: "external_replica", Path: path, LibraryID: m.LibraryID, State: "pending_link"}
	if err := r.DB.UpsertReplica(current); err != nil {
		return current, err
	}
	if err := r.ensureRecipientComponents(ctx, logicalID, m, current, asset, path, nil); err != nil {
		current.State = "error"
		current.Error = err.Error()
		_ = r.DB.UpsertReplica(current)
		return current, err
	}
	current.State = "pending_import"
	if err := r.DB.UpsertReplica(current); err != nil {
		return current, err
	}
	return current, errPendingImport
}

type Action struct {
	Kind           string
	AlbumID        string
	AlbumName      string
	MemberID       string
	SourceMemberID string
	LogicalAssetID string
	ImmichAssetID  string
	Error          string
}

func (a Action) Message() string {
	switch a.Kind {
	case "create_album_replica":
		return "dry-run: would create album for member"
	case "mapping_inconsistency":
		return "dry-run: unknown bridge or foreign asset needs review"
	case "unsupported_asset":
		return "dry-run: source asset cannot be shared"
	case "source_error":
		return "dry-run: source asset path needs review"
	case "source_missing":
		return "dry-run: previously observed asset is missing from Immich; sharing reference would be kept"
	case "discover_origin":
		return "dry-run: new source asset found in album"
	case "link_and_import":
		return "dry-run: would share asset with member through hardlink and import"
	case "add_sharing_reference":
		return "dry-run: would add asset to shared album membership"
	case "remove_sharing_reference":
		return "dry-run: would remove album sharing reference"
	case "add_album_asset":
		return "dry-run: would add member asset to album"
	case "remove_album_asset":
		return "dry-run: would remove member asset from album"
	case "delete_recipient_replica":
		return "dry-run: would delete unshared recipient asset and hardlinks"
	case "update_album_metadata":
		return "dry-run: would update member album name or description"
	case "update_album_cover":
		return "dry-run: would update member album cover"
	case "adopt_album_cover":
		return "dry-run: would use the member album cover for all mirror albums"
	case "album_cover_conflict":
		return "dry-run: conflicting member album cover needs review; the first configured member wins"
	default:
		return "dry-run: proposed action"
	}
}

func (r *Reconciler) DryRun(ctx context.Context) ([]Action, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	albums, err := r.DB.Albums()
	if err != nil {
		return nil, err
	}
	return r.previewPlans(ctx, albums)
}

func (r *Reconciler) Check(ctx context.Context) error {
	for _, m := range r.C.Members {
		id, err := r.API.Me(ctx, m)
		if err != nil {
			return err
		}
		if id != m.UserID {
			return fmt.Errorf("member %s API key belongs to %s", m.ID, id)
		}
		permissions, err := r.API.Permissions(ctx, m)
		if err != nil {
			return fmt.Errorf("member %s API key permissions: %w", m.ID, err)
		}
		required := []string{"stack.read", "stack.create", "stack.delete"}
		if r.C.RemoveUnshared {
			required = append(required, "asset.delete")
		}
		granted := map[string]bool{}
		for _, permission := range permissions {
			granted[permission] = true
		}
		for _, permission := range required {
			if !granted[permission] && !granted["all"] {
				return fmt.Errorf("member %s API key requires %s", m.ID, permission)
			}
		}
		library, err := r.API.GetLibrary(ctx, m)
		if err != nil {
			return fmt.Errorf("member %s library: %w", m.ID, err)
		}
		expected := filepath.Join(r.C.ImmichBridgeRoot, "families", r.C.FamilyID, "users", m.ID, "assets")
		if library.ID != m.LibraryID || library.OwnerID != m.UserID || len(library.ImportPaths) != 1 || library.ImportPaths[0] != expected {
			return fmt.Errorf("member %s library identity or import path mismatch", m.ID)
		}
	}
	return nil
}

func (r *Reconciler) mappedAsset(asset domain.Asset) (domain.Asset, error) {
	path, err := r.C.SourcePath(asset.OriginalPath)
	if err != nil {
		return asset, err
	}
	asset.OriginalPath = path
	if asset.SidecarPath != "" {
		sidecar, err := r.C.SourcePath(asset.SidecarPath)
		if err != nil {
			return asset, fmt.Errorf("map sidecar path: %w", err)
		}
		asset.SidecarPath = sidecar
	}
	return asset, nil
}

func (r *Reconciler) component(kind, source, recipient, state string) (domain.MediaComponent, error) {
	info, err := r.FS.SourceInfo(source)
	if err != nil {
		return domain.MediaComponent{}, err
	}
	return domain.MediaComponent{Kind: kind, SourcePath: source, RecipientPath: recipient, State: state, SourceSize: info.Size(), SourceMtimeNS: info.ModTime().UnixNano()}, nil
}

func (r *Reconciler) recordOriginComponents(logicalID, memberID string, asset domain.Asset) error {
	media, err := r.component("original", asset.OriginalPath, asset.OriginalPath, "ready")
	if err != nil {
		return err
	}
	if err := r.DB.UpsertReplicaFile(logicalID, memberID, media); err != nil {
		return err
	}
	if asset.SidecarPath == "" {
		return nil
	}
	sidecar, err := r.component("sidecar", asset.SidecarPath, asset.SidecarPath, "ready")
	if err != nil {
		return err
	}
	return r.DB.UpsertReplicaFile(logicalID, memberID, sidecar)
}

func (r *Reconciler) ensureRecipientComponents(ctx context.Context, logicalID string, member domain.Member, replica domain.Replica, asset domain.Asset, mediaPath string, remote *domain.Asset) error {
	if err := r.FS.Ensure(asset.OriginalPath, mediaPath); err != nil {
		return err
	}
	media, err := r.component("original", asset.OriginalPath, mediaPath, "ready")
	if err != nil {
		return err
	}
	if err := r.DB.UpsertReplicaFile(logicalID, member.ID, media); err != nil {
		return err
	}
	if asset.SidecarPath == "" {
		return nil
	}
	sidecarPath, err := r.FS.SidecarDestination(mediaPath, asset.SidecarPath)
	if err != nil {
		return err
	}
	if err := r.FS.Ensure(asset.SidecarPath, sidecarPath); err != nil {
		return err
	}
	state := "pending_import"
	component, err := r.component("sidecar", asset.SidecarPath, sidecarPath, state)
	if err != nil {
		return err
	}
	previous, err := r.DB.ReplicaFiles(logicalID, member.ID)
	if err != nil {
		return err
	}
	if remote != nil {
		expectedRemote, err := r.remoteBridgePath(sidecarPath)
		if err != nil {
			return err
		}
		if remote.SidecarPath != expectedRemote {
			component.State = "pending_discovery"
			r.sidecarDiscoveryNeeded = true
		} else {
			component.State = "ready"
			old, found := previous["sidecar"]
			changed := found && old.State == "ready" && old.SourcePath == component.SourcePath && (old.SourceSize != component.SourceSize || old.SourceMtimeNS != component.SourceMtimeNS)
			if changed {
				if replica.AssetID == "" {
					return errors.New("ready sidecar has no recipient asset ID")
				}
				if err := r.API.RefreshMetadata(ctx, member, []string{replica.AssetID}); err != nil {
					return fmt.Errorf("refresh recipient sidecar metadata: %w", err)
				}
			}
		}
	}
	return r.DB.UpsertReplicaFile(logicalID, member.ID, component)
}

func (r *Reconciler) remoteBridgePath(localPath string) (string, error) {
	rel, err := filepath.Rel(r.C.BridgeRoot, localPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("component path outside bridge root")
	}
	return filepath.Join(r.C.ImmichBridgeRoot, rel), nil
}

func (r *Reconciler) rebindOriginPath(logicalID, memberID, oldPath, newPath string) error {
	newInfo, err := r.FS.SourceInfo(newPath)
	if err != nil {
		return err
	}
	anchored := false
	oldInfo, err := r.FS.SourceInfo(oldPath)
	if err == nil {
		if !os.SameFile(oldInfo, newInfo) {
			return errors.New("new source has a different inode")
		}
		anchored = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	replicas, err := r.DB.ReplicasFor(logicalID)
	if err != nil {
		return err
	}
	for _, rep := range replicas {
		if rep.Role != "external_replica" || rep.Path == "" {
			continue
		}
		info, err := os.Lstat(rep.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !os.SameFile(info, newInfo) {
			return errors.New("recipient link does not match moved source inode")
		}
		if err := r.FS.Ensure(newPath, rep.Path); err != nil {
			return err
		}
		anchored = true
	}
	if !anchored {
		return errors.New("cannot verify moved source inode from existing links")
	}
	return r.DB.UpdateOriginPath(logicalID, memberID, oldPath, newPath)
}

package domain

import "path/filepath"

type Member struct {
	ID        string `json:"id" yaml:"id"`
	UserID    string `json:"userId" yaml:"user_id"`
	LibraryID string `json:"libraryId" yaml:"library_id"`
	KeyEnv    string `json:"-" yaml:"key_env"`
	Key       string `json:"-" yaml:"-"`
}

type Asset struct {
	ID               string `json:"id"`
	OwnerID          string `json:"ownerId"`
	LibraryID        string `json:"libraryId"`
	OriginalPath     string `json:"originalPath"`
	OriginalFileName string `json:"originalFileName"`
	Type             string `json:"type"`
	LivePhotoVideoID string `json:"livePhotoVideoId"`
	Stacked          bool   `json:"stacked"`
	Edited           bool   `json:"edited"`
	Sidecar          bool   `json:"sidecar"`
}

func (a Asset) Supported() bool {
	if a.Type != "IMAGE" && a.Type != "VIDEO" {
		return false
	}
	if a.LivePhotoVideoID != "" || a.Stacked || a.Edited || a.Sidecar {
		return false
	}
	return filepath.Ext(a.OriginalPath) != ""
}

type Album struct {
	ID          string `json:"id"`
	OwnerID     string `json:"ownerId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CoverID     string `json:"coverId"`
}

type Library struct {
	ID          string   `json:"id"`
	OwnerID     string   `json:"ownerId"`
	ImportPaths []string `json:"importPaths"`
}

type LogicalAsset struct {
	ID           string `json:"id"`
	OriginMember string `json:"originMember"`
	OriginAsset  string `json:"originAsset"`
}

type Replica struct {
	LogicalID string `json:"logicalId"`
	MemberID  string `json:"memberId"`
	AssetID   string `json:"assetId"`
	Path      string `json:"path"`
	LibraryID string `json:"libraryId"`
	Role      string `json:"role"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

// MediaComponent leaves room for paired videos and sidecars. The first
// milestone only reconciles a single original component per asset.
type MediaComponent struct {
	Kind          string `json:"kind"`
	SourcePath    string `json:"sourcePath"`
	RecipientPath string `json:"recipientPath"`
}

type LogicalAlbum struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CoverID     string `json:"coverLogicalAssetId,omitempty"`
	Initialized bool   `json:"initialized"`
}

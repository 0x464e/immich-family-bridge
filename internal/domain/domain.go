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
	SidecarPath      string `json:"sidecarPath,omitempty"`
}

func (a Asset) Supported() bool {
	return a.UnsupportedReason() == ""
}

func (a Asset) UnsupportedReason() string {
	if a.Type != "IMAGE" && a.Type != "VIDEO" {
		return "unsupported_type"
	}
	if a.LivePhotoVideoID != "" {
		return "live_photo"
	}
	if a.Stacked {
		return "stack"
	}
	if a.Edited {
		return "edit"
	}
	if filepath.Ext(a.OriginalPath) == "" {
		return "missing_extension"
	}
	if a.Sidecar && a.SidecarPath == "" {
		return "sidecar_path_missing"
	}
	if a.SidecarPath != "" && !equalFoldExt(a.SidecarPath, ".xmp") {
		return "unsupported_sidecar"
	}
	return ""
}

func equalFoldExt(path, ext string) bool {
	got := filepath.Ext(path)
	if len(got) != len(ext) {
		return false
	}
	for i := range got {
		a, b := got[i], ext[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
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

type MediaComponent struct {
	Kind          string `json:"kind"`
	SourcePath    string `json:"sourcePath"`
	RecipientPath string `json:"recipientPath"`
	State         string `json:"state"`
	SourceSize    int64  `json:"sourceSize"`
	SourceMtimeNS int64  `json:"sourceMtimeNs"`
}

type LogicalAlbum struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CoverID     string `json:"coverLogicalAssetId,omitempty"`
	Initialized bool   `json:"initialized"`
	SystemKey   string `json:"systemKey,omitempty"`
}

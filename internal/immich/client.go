package immich

import (
	"context"
	"errors"
	"github.com/0x464e/immich-family-bridge/internal/domain"
)

var ErrNotFound = errors.New("Immich resource not found")

type Client interface {
	Version(context.Context) (string, error)
	Me(context.Context, domain.Member) (string, error)
	Permissions(context.Context, domain.Member) ([]string, error)
	GetLibrary(context.Context, domain.Member) (domain.Library, error)
	GetAsset(context.Context, domain.Member, string) (domain.Asset, error)
	GetAlbum(context.Context, domain.Member, string) (domain.Album, error)
	ListAlbums(context.Context, domain.Member) ([]domain.Album, error)
	ListAlbumAssets(context.Context, domain.Member, string) ([]domain.Asset, error)
	CreateAlbum(context.Context, domain.Member, string, string, string) (domain.Album, error)
	UpdateAlbum(context.Context, domain.Member, string, string, string, string) error
	AddAssets(context.Context, domain.Member, string, []string) error
	RemoveAssets(context.Context, domain.Member, string, []string) error
	DeleteAssets(context.Context, domain.Member, []string) error
	ScanLibrary(context.Context, domain.Member) error
	DiscoverSidecars(context.Context) error
	RefreshMetadata(context.Context, domain.Member, []string) error
	FindByPath(context.Context, domain.Member, string) ([]domain.Asset, error)
}

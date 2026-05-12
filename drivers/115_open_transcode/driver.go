package _115_open_transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	stdpath "path"
	"strconv"
	"strings"
	"time"

	sdk "github.com/OpenListTeam/115-sdk-go"
	_115_open "github.com/OpenListTeam/OpenList/v4/drivers/115_open"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

// Link expiration for VideoPlay m3u8 URLs — long TTL since the player
// should fetch the m3u8 once and then hit the CDN directly for .ts segments.
var videoPlayLinkExpiration = 2 * time.Hour

// Short expiration for fallback download links (so stale/error results don't persist)
var fallbackLinkExpiration = 1 * time.Minute

var videoExts = []string{".mp4", ".mkv", ".avi", ".flv", ".wmv", ".ts", ".rmvb", ".webm", ".mov", ".m4v", ".mpg", ".mpeg"}

type Open115Transcode struct {
	model.Storage
	Addition
}

func (d *Open115Transcode) Config() driver.Config {
	return config
}

func (d *Open115Transcode) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Open115Transcode) Init(ctx context.Context) error {
	d.SourcePath = strings.TrimRight(d.SourcePath, "/")
	if d.SourcePath == "" {
		return fmt.Errorf("source_path is required")
	}
	_, err := fs.GetStorage(d.SourcePath, &fs.GetStoragesArgs{})
	if err != nil {
		return fmt.Errorf("source storage not found at %s: %w", d.SourcePath, err)
	}
	return nil
}

func (d *Open115Transcode) Drop(ctx context.Context) error {
	return nil
}

func (d *Open115Transcode) GetRoot(ctx context.Context) (model.Obj, error) {
	obj, err := fs.Get(ctx, d.SourcePath, &fs.GetArgs{NoLog: true})
	if err != nil {
		return nil, err
	}
	return &model.Object{
		Name:     "root",
		Path:     d.SourcePath,
		IsFolder: true,
		Modified: obj.ModTime(),
	}, nil
}

func (d *Open115Transcode) Get(ctx context.Context, path string) (model.Obj, error) {
	srcPath := d.srcPath(path)
	obj, err := fs.Get(ctx, srcPath, &fs.GetArgs{NoLog: true})
	if err != nil {
		// If path ends with .m3u8, try finding the original video file
		if strings.HasSuffix(strings.ToLower(path), ".m3u8") {
			basePath := srcPath[:len(srcPath)-5] // strip .m3u8
			for _, ext := range videoExts {
				obj2, err2 := fs.Get(ctx, basePath+ext, &fs.GetArgs{NoLog: true})
				if err2 == nil {
					return d.wrapObj(obj2, basePath+ext), nil
				}
			}
		}
		return nil, err
	}
	return d.wrapObj(obj, srcPath), nil
}

func (d *Open115Transcode) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	srcPath := dir.GetPath()
	if srcPath == "" {
		srcPath = d.SourcePath
	}
	objs, err := fs.List(ctx, srcPath, &fs.ListArgs{NoLog: true, Refresh: args.Refresh})
	if err != nil {
		return nil, err
	}
	result := make([]model.Obj, 0, len(objs))
	for _, obj := range objs {
		childSrcPath := stdpath.Join(srcPath, obj.GetName())
		result = append(result, d.wrapObj(obj, childSrcPath))
	}
	return result, nil
}

func (d *Open115Transcode) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	srcPath := file.GetPath()
	name := file.GetName()

	// Non-video files (and not .m3u8 rewritten ones): pass through to source storage
	if !isVideo(name) && !strings.HasSuffix(strings.ToLower(name), ".m3u8") {
		link, _, err := d.sourceLink(ctx, srcPath, args)
		return link, err
	}

	// Video files: get pick_code from source 115 Open, then call VideoPlay
	storage, actualPath, err := op.GetStorageAndActualPath(srcPath)
	if err != nil {
		return nil, fmt.Errorf("get source storage failed: %w", err)
	}

	obj, err := op.Get(ctx, storage, actualPath)
	if err != nil {
		return nil, fmt.Errorf("get source obj failed: %w", err)
	}

	open115Obj, ok := obj.(*_115_open.Obj)
	if !ok {
		log.Printf("[115_open_transcode] obj type=%T not *Obj, fallback", obj)
		return d.directSourceLink(ctx, storage, actualPath, args)
	}

	pc := open115Obj.Pc
	if pc == "" {
		log.Printf("[115_open_transcode] empty pick_code for %s, fallback", name)
		return d.directSourceLink(ctx, storage, actualPath, args)
	}

	open115Driver, ok := storage.(*_115_open.Open115)
	if !ok {
		log.Printf("[115_open_transcode] driver type=%T not *Open115, fallback", storage)
		return d.directSourceLink(ctx, storage, actualPath, args)
	}

	client := open115Driver.GetClient()
	if client == nil {
		return nil, fmt.Errorf("115 client not available")
	}

	playResp, err := videoPlayRaw(ctx, client, pc)
	if err != nil {
		log.Printf("[115_open_transcode] VideoPlay failed %s pc=%s: %v, fallback", name, pc, err)
		return d.directSourceLink(ctx, storage, actualPath, args)
	}

	if playResp == nil || len(playResp.VideoURL) == 0 || playResp.VideoURL[0].URL == "" {
		log.Printf("[115_open_transcode] VideoPlay empty for %s pc=%s, fallback", name, pc)
		return d.directSourceLink(ctx, storage, actualPath, args)
	}

	// Pick highest quality by definition number (higher = better: 4=1080P, 5=4K)
	bestIdx := 0
	for i, v := range playResp.VideoURL {
		if v.Definition > playResp.VideoURL[bestIdx].Definition {
			bestIdx = i
		}
	}
	log.Printf("[115_open_transcode] VideoPlay success %s pc=%s defs=%d using=def%d", name, pc, len(playResp.VideoURL), playResp.VideoURL[bestIdx].Definition)
	return &model.Link{
		URL: playResp.VideoURL[bestIdx].URL,
		Header: http.Header{
			"User-Agent": []string{"Mozilla/5.0"},
		},
		Expiration: &videoPlayLinkExpiration,
	}, nil
}

// --- helpers ---

func (d *Open115Transcode) srcPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return d.SourcePath
	}
	return stdpath.Join(d.SourcePath, path)
}

// videoName rewrites video file extensions to .m3u8 so players treat them
// as HLS streams and fetch .ts segments directly from the CDN instead of
// repeatedly hitting OpenList for 302 redirects.
func videoName(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range videoExts {
		if strings.HasSuffix(lower, ext) {
			return name[:len(name)-len(ext)] + ".m3u8"
		}
	}
	return name
}

// originalVideoName reverses the .m3u8 rewrite — tries each video extension
// against the source storage until it finds the real file.
func (d *Open115Transcode) originalVideoName(ctx context.Context, m3u8Name string) string {
	if !strings.HasSuffix(strings.ToLower(m3u8Name), ".m3u8") {
		return m3u8Name
	}
	base := m3u8Name[:len(m3u8Name)-5] // strip .m3u8
	return base                         // caller will try to resolve
}

func (d *Open115Transcode) wrapObj(obj model.Obj, srcPath string) model.Obj {
	name := obj.GetName()
	if !obj.IsDir() && isVideo(name) {
		name = videoName(name)
		// srcPath keeps the original path for source resolution
	}
	return &model.Object{
		Name:     name,
		Path:     srcPath,
		Size:     obj.GetSize(),
		Modified: obj.ModTime(),
		IsFolder: obj.IsDir(),
		HashInfo: obj.GetHash(),
	}
}

// sourceLink uses op.Link which goes through the link cache (used for non-video files).
func (d *Open115Transcode) sourceLink(ctx context.Context, srcPath string, args model.LinkArgs) (*model.Link, model.Obj, error) {
	storage, actualPath, err := op.GetStorageAndActualPath(srcPath)
	if err != nil {
		return nil, nil, err
	}
	return op.Link(ctx, storage, actualPath, model.LinkArgs{
		Header: args.Header,
		Type:   args.Type,
	})
}

// directSourceLink calls the underlying storage driver's Link() directly,
// bypassing op.Link's cache. This is used for video fallback so we always get
// a fresh download URL instead of a potentially stale cached one.
func (d *Open115Transcode) directSourceLink(ctx context.Context, storage driver.Driver, actualPath string, args model.LinkArgs) (*model.Link, error) {
	file, err := op.Get(ctx, storage, actualPath)
	if err != nil {
		return nil, fmt.Errorf("get file for fallback failed: %w", err)
	}
	link, err := storage.Link(ctx, file, model.LinkArgs{
		Header: args.Header,
		Type:   args.Type,
	})
	if err != nil {
		return nil, fmt.Errorf("fallback link failed: %w", err)
	}
	// Set short expiration so stale fallback links don't persist in cache
	link.Expiration = &fallbackLinkExpiration
	return link, nil
}

func isVideo(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range videoExts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// videoPlayRaw calls VideoPlay with relaxed JSON types (115 returns file_size as string)
type videoPlayRawResp struct {
	FileID   string             `json:"file_id"`
	FileName string             `json:"file_name"`
	FileSize json.Number        `json:"file_size"`
	Duration json.Number        `json:"duration"`
	Width    json.Number        `json:"width"`
	Height   json.Number        `json:"height"`
	VideoURL []sdk.VideoPlayURL `json:"video_url"`
}

func videoPlayRaw(ctx context.Context, client *sdk.Client, pickCode string) (*sdk.VideoPlayResp, error) {
	var raw videoPlayRawResp
	resp, err := client.AuthRequest(ctx, sdk.ApiVideoPlay, http.MethodGet, &raw, sdk.ReqWithQuery(sdk.Form{
		"pick_code": pickCode,
	}))
	if err != nil {
		return nil, err
	}
	// Extra safety: check if raw response contains error indicators not caught by SDK
	if resp != nil && raw.FileID == "" && raw.FileName == "" && len(raw.VideoURL) == 0 {
		return nil, fmt.Errorf("empty VideoPlay response (possible silent error), raw=%s", resp.String())
	}
	fileSize, _ := raw.FileSize.Int64()
	duration, _ := raw.Duration.Int64()
	width, _ := strconv.Atoi(raw.Width.String())
	height, _ := strconv.Atoi(raw.Height.String())
	return &sdk.VideoPlayResp{
		FileID:   raw.FileID,
		FileName: raw.FileName,
		FileSize: fileSize,
		Duration: duration,
		Width:    width,
		Height:   height,
		VideoURL: raw.VideoURL,
	}, nil
}

var _ driver.Driver = (*Open115Transcode)(nil)

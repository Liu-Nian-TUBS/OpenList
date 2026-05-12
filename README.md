# OpenList Fork — Liu-Nian-TUBS/OpenList

基于 [OpenListTeam/OpenList](https://github.com/OpenListTeam/OpenList) 上游的 fork，添加了 **115 Open Transcode** 驱动。

## 相对上游的全部改动

### 1. `drivers/115_open/driver.go` — 导出 SDK Client

新增 `GetClient()` 方法，让其他 driver（如 115 Open Transcode）可以获取底层的 115 SDK Client 实例。

```go
func (d *Open115) GetClient() *sdk.Client {
    return d.client
}
```

### 2. `drivers/115_open_transcode/` — 新增 115 Open Transcode 驱动

独立驱动，代理已有的 115 Open 存储，视频文件走 115 VideoPlay API 返回 m3u8 转码流。

**文件**：
- `meta.go` — 驱动注册，配置字段 `source_path`（指向已有的 115 Open 挂载路径，如 `/115`）
- `driver.go` — 核心逻辑

**工作方式**：
- **目录浏览**：透传到 source 115 Open 存储（List/Get 代理）
- **视频文件**：提取 `pick_code` → 调 `VideoPlay` API → 返回 m3u8 转码流 URL
- **非视频文件**：透传到 source 存储的直链
- **VideoPlay 失败时**：自动 fallback 到普通下载直链

**技术细节**：
- 115 的 VideoPlay API 返回的 `file_size` 是 string 而非 int，用 `json.Number` 宽松解析
- `sdk.VideoPlayURL` 包含多个清晰度，默认取 `[0]`（最高清晰度）
- VideoPlay m3u8 链接设置 **10 分钟缓存过期**（链接内含时间限制 token）
- Fallback 下载直链设置 **1 分钟缓存过期**（避免错误结果长期缓存）
- Fallback 通过 `directSourceLink()` **绕过 `op.Link` 缓存**，直接调底层 storage driver 的 `Link()` 方法，确保每次拿到新鲜 URL

**为什么 fallback 要绕过缓存**：
`op.Link()` 有全局 link cache。如果 115 遇到风控（如 911 账号异常），VideoPlay 失败，fallback 走 `op.Link` 会命中 115 Open storage 已缓存的旧链接（可能已过期）。直接调 `storage.Link()` 可以避免这个问题。

### 3. `drivers/all.go` — 注册新驱动

```go
import _ "github.com/OpenListTeam/OpenList/v4/drivers/115_open_transcode"
```

### 4. `.github/workflows/ghcr.yml` — GHCR CI

推送到 GitHub Container Registry 的 CI workflow。

**镜像地址**：`ghcr.io/liu-nian-tubs/openlist:main`

## 使用方法

1. 确保已有一个 `115 Open` 存储（如挂载在 `/115`）
2. OpenList 后台 → 添加存储 → 选择 `115 Open Transcode`
3. `source_path` 填 `/115`（即已有 115 Open 存储的挂载路径）
4. 挂载路径填 `/115Trans`（或任意名称）

访问 `/115Trans` 下的视频文件时，会自动走 115 转码流播放。

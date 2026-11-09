# 媒体切换、备份与恢复

本文对应 `cmd/gostream-media` 和 `internal/platform/media_transfer.go` 的当前实现。命令只处理 GoStream 专用 MinIO 桶中的 `media/` 与 `chunks/` 对象。媒体 URL 仍为 `/media/<生成文件名>`；工具不会修改 SQL 行，也不会删除本地文件。所有步骤要求先停止 API/worker 写入，并在同一停写窗口内取得 SQL 与对象快照。

## 运行前检查

使用 `go.mod` 指定的 Go 1.27.2。确认源数据库、`MEDIA_DIR` 和目标 MinIO 桶均属于同一部署；记录其名称与备份目录。预留容纳完整媒体目录和对象备份的空间。备份目录必须是尚不存在的新路径，工具最后才写入 `manifest.json`；没有该文件的目录是不完整备份。`restore` 会先校验清单、每个本地文件的 SHA-256、目标桶中是否有额外对象及同名对象的内容/MIME，再上传缺失对象。中断后可重复执行；同名对象内容冲突会报错，不会覆盖。`backup` 不会自动创建拼错名字的桶。

## 从现有本地媒体切到 MinIO

以下示例沿用当前 Compose 项目和原 `media_data` 卷。先自行保留 MySQL 与媒体卷的独立备份，并确认 `.env` 中已有稳定的 `JWT_SECRET`；切换期间不要更换密钥，否则现有登录令牌会失效。操作期间停止 API；MySQL、Redis 继续运行。运行目录为仓库根目录。

```sh
docker compose -f docker-compose.yml -f docker-compose.object.yml build api minio
docker compose stop api
docker compose -f docker-compose.yml -f docker-compose.object.yml up -d --wait minio
docker compose -f docker-compose.yml -f docker-compose.object.yml run --rm --no-deps \
  --entrypoint /usr/local/bin/gostream-media api import-local --dir /app/data/media
docker compose -f docker-compose.yml -f docker-compose.object.yml up -d --no-build api
```

导入会扫描 `media_assets`、视频媒体/封面、用户头像、未完成处理任务源 URL，以及仍有效的上传会话中的已登记分片。分片从 `MEDIA_DIR.chunks/<会话ID>/<序号>.part` 读取，并用 SQL 中的大小与 SHA-256 核验。每个本地文件必须存在且为普通文件；已有对象须与本地 SHA-256 一致。输出 JSON 的 `objects` 是去重后的对象数，`copied` 是本次写入对象数，`existing` 是已有且验证一致的对象数。命令返回错误时保持 API 停止，修复缺失文件或对象冲突后重跑。切换后用一个已发布旧视频和一个未完成上传会话实测 `/media`、Range、Feed 和续传完成，再开放写入。保留原媒体卷及相邻的 `.chunks` 目录；对象模式运行期间产生的新文件不会自动同步回本地卷，因此回退到本地模式前必须另行恢复对应快照。

## 一致的数据库与对象备份

备份整个部署时先停止 API/worker，保留 MySQL 和 MinIO 服务。以下示例在主机上执行媒体工具；先设置 `MINIO_ENDPOINT=127.0.0.1:9000`、`MINIO_ACCESS_KEY`、`MINIO_SECRET_KEY`、`MINIO_BUCKET`，并将备份目录存放在受访问控制的磁盘。`backup` 不需要数据库连接。

```sh
mkdir -m 700 /secure-backups/gostream-20261024
docker compose exec -T mysql sh -c \
  'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysqldump -uroot --single-transaction --routines --triggers --no-tablespaces --set-gtid-purged=OFF gostream' \
  > /secure-backups/gostream-20261024/mysql.sql
go run ./cmd/gostream-media backup --dir /secure-backups/gostream-20261024/objects
sha256sum /secure-backups/gostream-20261024/mysql.sql
```

`objects/manifest.json` 记录每个对象的键、字节数、SHA-256 和 Content-Type。工具备份全部最终媒体与未完成分片，并核对复制前后对象的 ETag 与大小。上述 SQL 和对象快照在停写窗口内对应同一逻辑状态；运行中的在线写入不在这套流程保证范围内。将整个备份目录和 SQL 校验值一同离机保存，并限制对 SQL、媒体和 MinIO 密钥的访问。

## 恢复到新数据库与空桶

准备新 MySQL 数据库与 MinIO 桶，先恢复 SQL，再恢复对象，最后启动 API。不要让后台媒体清理 worker 在对象恢复前运行。下面以已经创建且为空的 `gostream` 数据库为例，MinIO 环境变量改为**目标**服务和桶：

```sh
docker compose exec -T mysql sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" exec mysql -uroot gostream' \
  < /secure-backups/gostream-20261024/mysql.sql
go run ./cmd/gostream-media restore --dir /secure-backups/gostream-20261024/objects
docker compose -f docker-compose.yml -f docker-compose.object.yml up -d --no-build api
```

恢复输出的 `copied` 与 `existing` 之和应等于 `objects`。用旧账号、视频 URL、Range 请求、Feed 和未完成上传会话核对数据库与对象。错误或中断时保持 API 停止，修复后重跑；不要删除源备份。此流程尚未测量生产规模的恢复时间、对象版本/跨地域复制，也没有在线零停机切换保证。

## 隔离演练

`tests/integration/media_transfer_smoke.py` 使用临时本地媒体和空 MySQL，先在本地模式发布视频并保存一个未完成上传分片，再离线导入 MinIO、对象备份、SQL 逻辑备份、分别恢复到新桶/新数据库，并在对象模式读取旧 URL、完成原上传会话。脚本拒绝非空 MySQL 源库。下面的 `down -v` **只**用于专门的 `gostream-transfer` 测试项目：

```sh
docker compose -p gostream-transfer -f docker-compose.yml -f docker-compose.object.yml \
  -f tests/integration/object-test.compose.yml up -d --wait minio mysql
python3 tests/integration/media_transfer_smoke.py --go-bin /usr/local/go/bin/go \
  --mysql-dsn 'gostream:gostream-local@tcp(127.0.0.1:13307)/gostream?charset=utf8mb4&parseTime=True&loc=Local'
docker compose -p gostream-transfer -f docker-compose.yml -f docker-compose.object.yml \
  -f tests/integration/object-test.compose.yml down -v
```

## 媒体格式矩阵与十视频恢复验收（G04 / A03，真实双实例对象栈已验收）

下表为在真实 Compose 双实例联合栈（MySQL 8.4、MinIO 对象存储、双 API 实例）上的实测执行记录。对应 `tests/integration/media_matrix_smoke.py --mode compose`，覆盖 MP4/MOV/WebM 容器、有音轨/无音轨、不同编解码、竖屏与 >4 MiB 跨实例分片大文件，并严格检验失败视频的隐私边界与作者恢复流程。

### 十视频矩阵规格与实测数据

运行入口：`python3 tests/integration/acceptance_stack.py --no-build -- python3 tests/integration/media_matrix_smoke.py --mode compose --report-file data/diagnostics/media_matrix_smoke_compose.json`。

| 序号 | 标识与测试场景 | 输入容器与编解码 | 分辨率与音轨 | 源字节 | 终态 | 错误码 | 处理耗时 | 产物视频字节 | 产物封面字节 |
|---|---|---|---|---|---|---|---|---|---|
| 01 | `01_mp4_h264_aac` 基准发布 | MP4 (libx264+aac) | 640x360，有音轨 | 21,258 B | `published` | - | 1.655s | 20,532 B | 5,621 B |
| 02 | `02_mp4_h264_noaudio` 纯视频流 | MP4 (libx264) | 640x360，无音轨 | 12,434 B | `published` | - | 1.646s | 9,722 B | 6,333 B |
| 03 | `03_webm_vp8_vorbis` WebM输入 | WebM (vp8+vorbis) | 320x240，有音轨 | 36,103 B | `published` | - | 2.056s | 22,210 B | 6,768 B |
| 04 | `04_webm_vp9_opus` 现代WebM | WebM (vp9+opus) | 640x360，有音轨 | 30,016 B | `published` | - | 2.053s | 26,145 B | 7,948 B |
| 05 | `05_mov_prores` 专业格式 | MOV (prores+pcm) | 480x270，有音轨 | 1,060,497 B | `published` | - | 1.676s | 24,656 B | 7,702 B |
| 06 | `06_mov_h264_aac` 常见QuickTime | MOV (libx264+aac) | 640x360，有音轨 | 20,290 B | `published` | - | 2.050s | 21,162 B | 7,895 B |
| 07 | `07_mp4_portrait` 竖屏短视频 | MP4 (libx264+aac) | 360x640 竖屏 | 19,437 B | `published` | - | 2.062s | 18,553 B | 10,644 B |
| 08 | `08_overlong_reject` 超长截断 | MP4 (libx264) | 16x16，605秒 | 9,345 B | `failed` | `duration_limit` | 1.244s | 0 B | 0 B |
| 09 | `09_corrupted_reject` 损坏载荷 | MP4 (ftyp头+坏载荷) | 坏流 | 16,024 B | `failed` | `processing_failed` | 25.940s (含重试) | 0 B | 0 B |
| 10 | `10_chunked_large_mp4` 跨实例大分片 | MP4 (libx264) | 720x480，4.44MB | 4,443,118 B | `published` | - | 2.673s | 2,436,727 B | 28,884 B |

### 关键断言与恢复机制

1. **失败隐私边界**：
   - 匿名访问 `GET /api/videos/{failed_id}` 与其他登录用户访问均严格返回 `404 video not found`。
   - 他人查看作者公开视频列表 `GET /api/users/{author_id}/videos` 时，失败视频自动过滤。
   - 公共搜索 `GET /api/search?q=...&type=videos` 与公共 Feed 均排除未发布/失败视频。
   - 仅作者本人携带令牌访问时可见 `processing_error` 字段（如 `duration_limit`、`processing_failed`）。
2. **作者修复恢复（Recovery）**：
   - 非作者账号通过 `PATCH /api/videos/{failed_id}` 试图替换媒体时，API 返回 `403 only the author can edit this video`。
   - 作者上传合法媒体后发起 `PATCH /api/videos/{failed_id}` 携带新 `media_url`，视频状态重置为 `processing` 并重新入队。
   - 后台重新完成转码与探测后，状态原子更新为 `published`，生成封面并在公共 Feed 与搜索中立即可见。
3. **分片续传与跨实例**：
   - 大于 4 MiB 视频（如用例 10）通过 `/api/uploads/sessions` 分片，分片分别打到 API A 和 API B，验证了跨实例分片状态与哈希组装一致性。

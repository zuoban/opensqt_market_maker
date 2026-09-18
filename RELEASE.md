# Release Guide

## 版本规范

- 版本号以 [main.go](main.go#L21) 中的 `Version` 为唯一来源，格式固定为 `v主版本.次版本.修订号`，例如 `v3.4.3`。
- Git 标签必须与 `Version` 完全一致。
- GitHub Release 名称、Tag 名称、发行包文件名必须与该版本号保持一致。
- [ARCHITECTURE.md](ARCHITECTURE.md) 顶部文档版本建议同步更新，避免文档和程序版本脱节。
- 如果某个标签已经发布，后续改动必须递增补丁版本，不要重打已有标签。
- 本地打包也以 `main.go` 为准；可传入同值 `VERSION`，但不允许通过环境变量改写发行版本。版本不一致或格式错误时，脚本会在编译及清理产物前退出。

## 构建环境

- 使用 Go 1.27.1 或更高的已修补版本；发布 CI 从 `go.mod` 选择版本，Docker 默认版本必须与其一致。
- Windows ZIP 使用 Python 3 标准库生成，正确保留中文路径的 UTF-8 标记；本地交叉打包 Windows 前需安装 `python3`，不再依赖系统 `zip`。
- 打包回归与主程序漏洞扫描均为共用 Checks 的发布门槛，失败时不发布附件或镜像。

## 发布流程

1. 修改 [main.go](main.go#L21) 中的版本号。
2. 同步更新 [ARCHITECTURE.md](ARCHITECTURE.md) 中的版本说明，并新增 `release-notes/<版本号>.md`，说明功能、兼容性及尚未实现的边界；工作流会将其用于 GitHub Release 正文。
3. 验证模块、编译和回归测试：

```bash
go mod verify
go build ./...
go vet ./...
go test -race ./... -count=1
node --test web/app_frontend_test.js
python3 -B -m unittest discover -s scripts -p 'test_package_release.py'
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 .
```

4. 提交代码并推送到主分支。
5. 创建并推送同名标签，例如：

```bash
git tag -a v3.4.3 -m "Release v3.4.3"
git push origin main
git push origin v3.4.3
```

6. 推送标签后，GitHub Actions 会自动：

- Release 与 Docker 工作流先通过共用的 Checks（依赖校验、静态检查、漏洞扫描、打包回归、并发回归、前端测试），失败时不发布
- 校验 [main.go](main.go#L21) 的版本号和 tag 一致
- 构建 Linux amd64、Windows amd64、MacOS arm64 三个平台附件
- 自动创建或更新 GitHub Release
- 自动上传压缩包和对应的 `.sha256` 文件
- 构建 `linux/amd64` 与 `linux/arm64` Docker 镜像，并推送到 GitHub Packages（GHCR）

镜像地址：

```text
ghcr.io/zuoban/opensqt_market_maker:v3.4.7
ghcr.io/zuoban/opensqt_market_maker:latest
```

首次发布的 Package 默认私有。如需公开拉取，到仓库 Packages 页面把可见性改为 Public。

7. 如需本地手动生成单个平台发行包：

```bash
./scripts/package_release.sh
```

也可以指定平台：

```bash
TARGET_OS=windows TARGET_ARCH=amd64 ./scripts/package_release.sh
TARGET_OS=MacOS TARGET_ARCH=arm64 ./scripts/package_release.sh
```

脚本对外推荐使用 `TARGET_OS` 和 `TARGET_ARCH`。内部仍会自动映射到 Go 的标准目标平台，例如 `MacOS -> darwin`。

## 发行包内容

脚本 [scripts/package_release.sh](scripts/package_release.sh) 会生成以下内容：

- 编译后的可执行文件 `opensqt_market_maker` 或 `opensqt_market_maker.exe`
- `live_server/` 中由 Git 明确跟踪的演示文件
- `config.example.yaml`
- `config.yaml`
- `.env.example`
- `README.md`
- `ARCHITECTURE.md`

## 安全说明

- 发布包中的 `config.yaml` 由 `config.example.yaml` 复制生成，不会使用你本机的真实 [config.yaml](config.yaml)。
- 这样可以保留开箱即用的目录结构，同时避免把 API Key 或私钥打进公开 Release。
- `live_server/` 只复制 Git 已跟踪文件，忽略文件和本机临时文件不会进入公开 Release。
- `部署教程.pdf` 仅作为本地资料保留并已加入 `.gitignore`；发布前必须确认它不再被 Git 跟踪，因此不会进入 tag、GitHub 自动生成的源码包或公开 Release。

## 推荐命名

- 发行包：`opensqt_market_maker_v3.4.3_linux_amd64.tar.gz`
- Windows 发行包：`opensqt_market_maker_v3.4.3_windows_amd64.zip`
- MacOS 发行包：`opensqt_market_maker_v3.4.3_MacOS_arm64.tar.gz`
- 校验文件：`opensqt_market_maker_v3.4.3_linux_amd64.tar.gz.sha256`

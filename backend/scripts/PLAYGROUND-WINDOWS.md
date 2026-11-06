# Windows Playground 构建

在 backend 目录执行：

```powershell
.\scripts\build-windows.ps1 -LlvmRoot E:\toolchains\llvm-mingw-20261020-ucrt-x86_64 -SpiderMonkeyRoot E:\toolchains\spidermonkey
```

使用与 AIStudio2API2 相同的 LLVM-MinGW 20261020 和 SpiderMonkey
`v152.0.4-aistudio2api.1`。可传入 `-DownloadDependencies` 下载这些固定版本。
脚本先检查语言键、类型并构建嵌入式前端，再检查头文件和两个静态库，以 CGO=1 构建，并执行实际 JS 上下文/表达式检查。
已有本轮前端产物时可传 `-SkipFrontend`。
产物为 `bin/server-playground-windows-amd64.exe`；旁边的 build.json 记录库和程序的 SHA256。

```powershell
.\scripts\build-windows.ps1 -WithoutPlayground
```

该入口明确生成 `server-no-playground-windows-amd64.exe`，无法执行网页 WAA。
任一产物可运行 `--check-playground-runtime` 检查运行时，缺失时非零退出。

## 可续签网页认证（默认）

账号认证已改为 Camoufox 浏览器登录，移除了 Cookie 粘贴和 Chrome RT 导入。

构建后使用 `--install-playground-browser` 安装匹配的 Camoufox，持久保存 DATA_DIR。完整部署和验证步骤见 [PLAYGROUND-CAMOUFOX.md](PLAYGROUND-CAMOUFOX.md)。

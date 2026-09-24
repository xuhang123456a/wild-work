@echo off
REM 本地一键构建（Windows 双击/命令行均可）。
REM 真正的逻辑在 build-local.sh，这里只负责找到 Git Bash 并转发参数。
REM 依赖：Git for Windows（提供 bash）、Go 1.25+。
REM 用法：build-local.bat          完整流程
REM       build-local.bat --fast   跳过测试

setlocal
where bash >nul 2>nul
if errorlevel 1 (
  echo [错误] PATH 中找不到 bash。
  echo         请安装 Git for Windows，或直接用 Git Bash 执行 build/build-local.sh
  exit /b 1
)

bash "%~dp0build-local.sh" %*
exit /b %ERRORLEVEL%

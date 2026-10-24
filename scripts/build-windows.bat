@echo off
setlocal EnableExtensions
cd /d "%~dp0.."

echo ========================================
echo BiliLottery Build (Windows amd64)
echo ========================================
echo.

where go >nul 2>nul
if errorlevel 1 (
    echo ERROR: Go was not found in PATH.
    pause
    exit /b 1
)
go version
if errorlevel 1 goto :error

echo.
echo [1/4] Downloading Go modules...
if "%GOPROXY%"=="" set "GOPROXY=https://goproxy.cn,direct"
go mod tidy
if errorlevel 1 goto :error

echo.
echo [2/4] Checking rsrc...
where rsrc >nul 2>nul
if errorlevel 1 (
    echo rsrc was not found. Installing...
    go install github.com/akavel/rsrc@latest
    if errorlevel 1 goto :error
)

rsrc -manifest app.manifest -o rsrc_windows_amd64.syso
if errorlevel 1 goto :error

if not exist rsrc_windows_amd64.syso (
    echo ERROR: rsrc_windows_amd64.syso was not generated.
    goto :error
)

echo.
echo [3/4] Building Windows EXE...
if not exist dist mkdir dist
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0

go build -trimpath -buildvcs=false -ldflags="-s -w -H=windowsgui" -o "dist\BiliLottery-windows-amd64.exe" .
if errorlevel 1 goto :error

if not exist "dist\BiliLottery-windows-amd64.exe" (
    echo ERROR: the output EXE was not generated.
    goto :error
)

rem 应用清单已写入二进制，清理中间产物，保持目录干净
del /q rsrc_windows_amd64.syso 2>nul

echo.
echo [4/4] Build complete.
echo.
echo ========================================
echo BUILD SUCCESS
echo ========================================
echo.
echo Output:
echo %CD%\dist\BiliLottery-windows-amd64.exe
echo.
echo The EXE starts a local server and opens the default browser.
echo.
pause
exit /b 0

:error
echo.
echo ========================================
echo BUILD FAILED
echo ========================================
echo.
pause
exit /b 1
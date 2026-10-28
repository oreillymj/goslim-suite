@echo off
rem Build GoSlimPlayer, GoSlim and GoUPnPAudioClient into dist\ (Windows and Linux, 64-bit and 32-bit).
rem
rem   hack\build.bat                 all apps
rem   hack\build.bat /?              show how to set the version
rem   set "VERSION=1.2.3" ^&^& hack\build.bat      (cmd)
rem   $env:VERSION = "1.2.3"; .\hack\build.bat   (PowerShell)
rem   set SKIP_DEPS=1 ^& hack\build.bat      skip go get / go mod tidy (offline)
rem
rem The Linux builds use -tags noaudio (UPnP and proxy modes only; no cgo). macOS and
rem Linux arm64 builds: use hack\build.sh from WSL or Git Bash.
setlocal
cd /d "%~dp0\.." || (echo Cannot change to the repository directory "%~dp0.." & goto :fail)

if /i "%~1"=="/?" goto :help
if /i "%~1"=="-h" goto :help
if /i "%~1"=="--help" goto :help
if /i "%~1"=="help" goto :help

if "%VERSION%"=="" for /f "delims=" %%i in ('git describe --tags --always --dirty 2^>nul') do set "VERSION=%%i"
if "%VERSION%"=="" set "VERSION=dev"
set "VERSION=%VERSION: =%"

echo Version is set to %VERSION%
echo Building in %CD%
if not exist go.mod (
  echo go.mod not found in %CD% - is hack\build.bat inside a complete clone of the repository?
  goto :fail
)
set "LDFLAGS=-s -w -X main.version=%VERSION%"

rem External dependencies: fetch them, then let go mod tidy write go.mod and go.sum.
if "%SKIP_DEPS%"=="1" goto :depsdone
for %%D in (github.com/hajimehoshi/go-mp3 github.com/mewkiz/flac github.com/ebitengine/oto/v3 github.com/jfreymuth/oggvorbis github.com/tphakala/go-aac/pcm golang.org/x/net) do (
  echo ==^> go get %%D
  go get %%D || goto :fail
)
echo ==^> go mod tidy
go mod tidy || goto :fail
:depsdone

if not exist dist mkdir dist

for %%A in (goslimplayer goslim goupnpaudioclient) do (
  call :build %%A amd64 || goto :fail
  call :build %%A 386 || goto :fail
)
echo Done. Output is in dist\ (version %VERSION%).
exit /b 0

:build
rem %1 = app, %2 = GOARCH. Local audio needs no cgo on Windows; the Linux build is UPnP/proxy only.
set "CGO_ENABLED=0"
set "GOARCH=%2"

set "GOOS=windows"
echo ==^> dist\%1-%VERSION%-windows-%2.exe
go build -trimpath -ldflags "%LDFLAGS%" -o "dist\%1-%VERSION%-windows-%2.exe" ".\cmd\%1"
if errorlevel 1 exit /b 1

set "GOOS=linux"
echo ==^> dist\%1-%VERSION%-linux-%2
go build -trimpath -tags noaudio -ldflags "%LDFLAGS%" -o "dist\%1-%VERSION%-linux-%2" ".\cmd\%1"
exit /b %errorlevel%

:fail
echo Build failed. Current directory: %CD%
exit /b 1

:help
echo Builds GoSlimPlayer, GoSlim and GoUPnPAudioClient into dist\ (Windows and Linux, 64-bit and 32-bit).
echo.
echo To build with version information
echo.
echo In a command prompt - cmd
echo   set "VERSION=0.5" ^&^& hack\build.bat
echo.
echo In a Powershell terminal
echo   $env:VERSION = "0.5"
echo   .\hack\build.bat
echo.
echo Without VERSION the version comes from "git describe", or is "dev".
echo To build offline from already downloaded dependencies: set SKIP_DEPS=1
exit /b 0

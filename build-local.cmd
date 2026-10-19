@echo off
rem Local build into dist\bh.exe inside the repo. Installing is the job of /update;
rem this script never writes to %LOCALAPPDATA%\BarrosoHarness\bin.
rem Usage: build-local.cmd [version]   (passed to the linker as -X main.Versao)
set VERSION=%~1
if "%VERSION%"=="" set VERSION=dev
go build -ldflags "-X main.Versao=%VERSION%" -o "%~dp0dist\bh.exe" ./cmd/barroso-harness || exit /b 1
"%~dp0dist\bh.exe" -version

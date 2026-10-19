@echo off
rem Versão da build, repassada ao linker (-X main.Versao).
set VERSAO=%~1
if "%VERSAO%"=="" set VERSAO=dev
go build -ldflags "-X main.Versao=%VERSAO%" -o "%LOCALAPPDATA%\BarrosoHarness\bin\bh.exe" ./cmd/barroso-harness || exit /b 1
bh -version

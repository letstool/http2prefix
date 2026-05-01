@echo off
cd /d "%~dp0.."
if not exist db mkdir db

REM Mode 1 (default): fetch CSV from CDN (set LICENSE_KEY for licensed access)
REM Mode 2 (peer):    set PREFIX_DB_URL=http://upstream-host:8080

set LISTEN_ADDR=127.0.0.1:8080
set PREFIX_DB_DIR=.\db
REM set LICENSE_KEY=your_token_here
.\out\http2prefix.exe

@echo off
go build ^
    -trimpath ^
    -ldflags="-s -w" ^
    -o .\out\http2prefix.exe .\cmd\http2prefix

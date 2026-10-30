param(
    [string]$Ffmpeg = '',
    [string]$Version = '1.0.0'
)

$ErrorActionPreference = 'Continue'
$root = $PSScriptRoot
$src = Join-Path $root 'hvconverter'
$setup = Join-Path $root 'hvsetup'
$dist = Join-Path $root 'dist'

if (-not $Ffmpeg) {
    $cmd = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue
    if ($cmd) { $Ffmpeg = $cmd.Source }
}
if (-not $Ffmpeg -or -not (Test-Path $Ffmpeg)) {
    Write-Host 'ERROR: ffmpeg.exe not found. Pass -Ffmpeg "C:\path\to\ffmpeg.exe"' -ForegroundColor Red
    exit 1
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host 'ERROR: go not found in PATH' -ForegroundColor Red
    exit 1
}

function Compress-File([string]$in, [string]$out) {
    if (-not (Test-Path $in)) { throw "missing input: $in" }
    New-Item -ItemType Directory -Force -Path (Split-Path $out -Parent) | Out-Null
    $inStream = [IO.File]::OpenRead($in)
    $outStream = [IO.File]::Create($out)
    $gz = New-Object IO.Compression.GZipStream($outStream, [IO.Compression.CompressionLevel]::Optimal)
    try { $inStream.CopyTo($gz) } finally { $gz.Dispose(); $outStream.Dispose(); $inStream.Dispose() }
}

New-Item -ItemType Directory -Force -Path $dist | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $src 'payload') | Out-Null
New-Item -ItemType Directory -Force -Path (Join-Path $setup 'payload') | Out-Null

Write-Host '=== 1/5 便携版载荷 ==='
Compress-File $Ffmpeg (Join-Path $src 'payload\ffmpeg.exe.gz')

Write-Host '=== 2/5 编译主程序 ==='
Push-Location $src
& go build -ldflags "-H=windowsgui -s -w" -o hvconverter.exe .
if ($LASTEXITCODE -ne 0) { Pop-Location; exit 1 }

Write-Host '=== 3/5 编译便携版 ==='
& go build -tags portable -ldflags "-H=windowsgui -s -w" -o HVConverter-Portable.exe .
if ($LASTEXITCODE -ne 0) { Pop-Location; exit 1 }
Pop-Location

Write-Host '=== 4/5 安装器载荷 ==='
Compress-File (Join-Path $src 'hvconverter.exe') (Join-Path $setup 'payload\app.exe.gz')
Compress-File $Ffmpeg (Join-Path $setup 'payload\ffmpeg.exe.gz')
Copy-Item (Join-Path $src 'app.ico') (Join-Path $setup 'payload\app.ico') -Force
Copy-Item (Join-Path $src 'app.manifest') (Join-Path $setup 'setup.manifest') -Force
Copy-Item (Join-Path $src 'app.ico') (Join-Path $setup 'app.ico') -Force

Push-Location $setup
& go build -o cleanup_build.exe ./cleanup
if ($LASTEXITCODE -ne 0) { Pop-Location; exit 1 }
Compress-File (Join-Path $setup 'cleanup_build.exe') (Join-Path $setup 'payload\cleanup.exe.gz')
Remove-Item (Join-Path $setup 'cleanup_build.exe') -Force -ErrorAction SilentlyContinue

Write-Host '=== 5/5 编译安装器 ==='
if (Get-Command rsrc.exe -ErrorAction SilentlyContinue) {
    & rsrc.exe -manifest setup.manifest -ico app.ico -o rsrc.syso
}
& go build -ldflags "-H=windowsgui -s -w" -o HVConverter-Setup.exe .
$rc = $LASTEXITCODE
Pop-Location
if ($rc -ne 0) { exit 1 }

Copy-Item (Join-Path $src 'hvconverter.exe') $dist -Force
Copy-Item (Join-Path $src 'HVConverter-Portable.exe') $dist -Force
Copy-Item (Join-Path $setup 'HVConverter-Setup.exe') $dist -Force

Write-Host ''
Write-Host "构建完成（v$Version）：" -ForegroundColor Green
Get-ChildItem $dist -Filter '*.exe' | ForEach-Object {
    Write-Host ("  {0,-28} {1} MB" -f $_.Name, [math]::Round($_.Length / 1MB, 1))
}

$ErrorActionPreference = 'Stop'
Push-Location (Join-Path $PSScriptRoot '..')
try {
    docker build --target artifacts --output type=local,dest=./output .
    if ($LASTEXITCODE -ne 0) { throw 'Docker build failed' }
} finally {
    Pop-Location
}

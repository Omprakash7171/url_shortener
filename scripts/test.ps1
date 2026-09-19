param(
    [string]$TEST_DATABASE_URL = "postgres://urlshortener:urlshortener_dev@localhost:5439/urlshortener_test",
    [string]$TEST_REDIS_ADDR = "localhost:6389",
    [switch]$SkipRedis
)

$ErrorActionPreference = "Stop"
$go = "C:\Program Files\Go\bin\go.exe"

$packages = @("./internal/service", "./internal/repository", "./internal/handler", "./internal/idempotency")

Write-Host "[test] compiling test binaries -> bin\"
foreach ($p in $packages) {
    $name = ($p -replace "\./internal/", "" -replace "/", "-")
    & $go "test" "-c" "-o" "bin\$name.test.exe" $p
    if ($LASTEXITCODE -ne 0) { throw "compile failed for $p" }
}

Write-Host "[test] running unit tests (service + base62)"
& ".\bin\service.test.exe"
if ($LASTEXITCODE -ne 0) { throw "service tests failed" }

Write-Host "[test] running idempotency store tests"
$env:TEST_REDIS_ADDR = $TEST_REDIS_ADDR
& ".\bin\idempotency.test.exe"
if ($LASTEXITCODE -ne 0) { throw "idempotency tests failed" }

Write-Host "[test] running repository integration tests"
$env:TEST_DATABASE_URL = $TEST_DATABASE_URL
& ".\bin\repository.test.exe"
if ($LASTEXITCODE -ne 0) { throw "repository tests failed" }

Write-Host "[test] running API integration tests"
& ".\bin\handler.test.exe"
if ($LASTEXITCODE -ne 0) { throw "handler tests failed" }

Write-Host "[test] ALL TESTS PASSED"
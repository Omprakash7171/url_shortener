param(
    [string]$TEST_DATABASE_URL = "postgres://urlshortener:urlshortener_dev@localhost:5439/urlshortener_test"
)

$ErrorActionPreference = "Stop"
$go = "C:\Program Files\Go\bin\go.exe"
$env:TEST_DATABASE_URL = $TEST_DATABASE_URL

# The service package is pure logic (no DB) -> coverage is meaningful without a database.
& $go "test" "-c" "-cover" "-covermode=atomic" "-o" "bin\coverage.test.exe" "./internal/service"
if ($LASTEXITCODE -ne 0) { throw "coverage compile failed" }

& ".\bin\coverage.test.exe" "-test.coverprofile=bin\coverage.out"
if ($LASTEXITCODE -ne 0) { throw "coverage run failed" }

& $go "tool" "cover" "-func=bin\coverage.out"
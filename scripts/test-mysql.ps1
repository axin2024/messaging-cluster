param(
    [Parameter(Mandatory)][string]$MySQLBin,
    [string]$PreviousBinary
)

$ErrorActionPreference = 'Stop'
$MySQLBin = (Resolve-Path -LiteralPath $MySQLBin).Path
if ($PreviousBinary) { $PreviousBinary = (Resolve-Path -LiteralPath $PreviousBinary).Path }

function New-LoopbackPort {
    $socket = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    try {
        $socket.Start()
        return $socket.LocalEndpoint.Port
    } finally {
        $socket.Stop()
    }
}

function Assert-Exit([string]$Operation) {
    if ($LASTEXITCODE -ne 0) { throw "$Operation exited with code $LASTEXITCODE" }
}

function Wait-Ready($Process, [string]$Origin) {
    for ($i = 0; $i -lt 20; $i++) {
        if ($Process.HasExited) { throw 'HTTP service exited' }
        try {
            if ((Invoke-RestMethod "$Origin/readyz" -TimeoutSec 2).ready) { return }
        } catch {}
        Start-Sleep -Milliseconds 250
    }
    throw 'HTTP service did not become ready'
}

function New-FixtureSession([string]$Tenant, [string]$Token, [string]$Username) {
    $password = 'synthetic-operator-password'
    $registration = @{ username = $Username; display_name = $Username; password = $password }
    $appHeaders = @{ Authorization = "Bearer $Token" }
    Invoke-RestMethod "$origin/v1/admin/users" -Method Post -ContentType 'application/json' -Headers $appHeaders -Body ($registration | ConvertTo-Json) | Out-Null
    $login = @{ username = $Username; password = $password; tenant = $Tenant; app = 'dispatch'; device_key = 'desk' }
    Invoke-RestMethod "$origin/v1/sessions" -Method Post -ContentType 'application/json' -Body ($login | ConvertTo-Json)
}

$fixtureKey = [guid]::NewGuid().ToString('N').Substring(0, 10)
$projectRoot = Split-Path $PSScriptRoot -Parent
$fixtureRoot = Join-Path $projectRoot "storage/mysql-$fixtureKey"
New-Item -ItemType Directory -Path "$fixtureRoot/data" -Force | Out-Null
$dbName = "messaging_test_$fixtureKey"
$dbPort = New-LoopbackPort
$httpPort = New-LoopbackPort
$adminPassword = [guid]::NewGuid().ToString('N')
$appPassword = [guid]::NewGuid().ToString('N')
$mysql = Join-Path $MySQLBin 'mysql.exe'
$mysqld = Join-Path $MySQLBin 'mysqld.exe'
$mysqladmin = Join-Path $MySQLBin 'mysqladmin.exe'
$adminArgs = @('--no-defaults', '--protocol=TCP', '--host=127.0.0.1', "--port=$dbPort", '--user=root')
$serverArgs = @('--no-defaults', '--innodb-buffer-pool-size=32M', '--max-connections=48', '--mysqlx=OFF',
    "--basedir=$(Split-Path $MySQLBin -Parent)", "--datadir=$fixtureRoot/data", "--log-error=$fixtureRoot/mysql.log")
$bootstrapFile = "$fixtureRoot/init.sql"
$dbProcess = $null
$appProcess = $null
$origin = "http://127.0.0.1:$httpPort"
$binary = "$fixtureRoot/messaging.exe"
$environment = @{}
foreach ($name in @('MYSQL_PWD', 'MESSAGING_TEST_MYSQL_DSN', 'MESSAGING_MYSQL_DSN', 'MESSAGING_HTTP_ADDR', 'MESSAGING_ATTACHMENT_DIR', 'MESSAGING_NODE_NAME')) {
    $environment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

Push-Location -LiteralPath $projectRoot
try {
    $init = Start-Process -FilePath $mysqld -ArgumentList ($serverArgs + '--initialize-insecure') -PassThru -Wait -WindowStyle Hidden
    if ($init.ExitCode -ne 0) { throw 'MySQL initialization failed' }
    @"
ALTER USER 'root'@'localhost' IDENTIFIED BY '$adminPassword';
CREATE DATABASE $dbName CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
CREATE USER 'messaging_test'@'127.0.0.1' IDENTIFIED BY '$appPassword';
GRANT ALL PRIVILEGES ON $dbName.* TO 'messaging_test'@'127.0.0.1';
"@ | Set-Content -LiteralPath $bootstrapFile -Encoding ascii
    $dbProcess = Start-Process -FilePath $mysqld -ArgumentList ($serverArgs + @('--bind-address=127.0.0.1', "--port=$dbPort", "--init-file=$bootstrapFile")) -PassThru -WindowStyle Hidden
    $env:MYSQL_PWD = $adminPassword
    $version = $null
    for ($i = 0; $i -lt 40; $i++) {
        if ($dbProcess.HasExited) { throw 'MySQL exited before accepting connections' }
        $version = & $mysql @adminArgs '--batch' '--skip-column-names' '--execute=SELECT VERSION()' 2>$null
        if ($LASTEXITCODE -eq 0) { break }
        Start-Sleep -Milliseconds 250
    }
    if (!$version) { throw 'MySQL did not become ready' }
    Remove-Item -LiteralPath $bootstrapFile
    Write-Output "Isolated MySQL $version"
    $env:MESSAGING_TEST_MYSQL_DSN = "messaging_test:${appPassword}@tcp(127.0.0.1:$dbPort)/$dbName"
    $env:MESSAGING_MYSQL_DSN = $env:MESSAGING_TEST_MYSQL_DSN
    $env:MESSAGING_HTTP_ADDR = "127.0.0.1:$httpPort"
    $env:MESSAGING_ATTACHMENT_DIR = "$fixtureRoot/attachments"
    $env:MESSAGING_NODE_NAME = 'fixture'
    go build -o $binary ./cmd/messaging
    Assert-Exit 'build service'
    if ($PreviousBinary) {
        $oldVersion = & $PreviousBinary migrate | ConvertFrom-Json
        Assert-Exit 'previous migration'
        if ($oldVersion.schema_version -ne 5) { throw 'Expected a previous binary using schema version 5' }
        $oldApp = & $PreviousBinary create-app upgrade dispatch | ConvertFrom-Json
        Assert-Exit 'previous application bootstrap'
        $appProcess = Start-Process -FilePath $PreviousBinary -ArgumentList 'serve' -PassThru -WindowStyle Hidden -RedirectStandardError "$fixtureRoot/previous.log"
        Wait-Ready $appProcess $origin
        $oldSender = New-FixtureSession 'upgrade' $oldApp.admin_token 'sender'
        $oldRecipient = New-FixtureSession 'upgrade' $oldApp.admin_token 'recipient'
        $senderHeaders = @{ Authorization = 'Bearer ' + $oldSender.token }
        $recipientHeaders = @{ Authorization = 'Bearer ' + $oldRecipient.token }
        $direct = @{ user_id = $oldRecipient.session.user_id } | ConvertTo-Json
        $oldChat = Invoke-RestMethod "$origin/v1/conversations/direct" -Method Post -Headers $senderHeaders -ContentType 'application/json' -Body $direct
        $oldPath = "$origin/v1/conversations/$($oldChat.id)"
        $oldPayload = @{ client_id = 'before-upgrade'; body = 'first dispatch' } | ConvertTo-Json
        $oldReceipt = Invoke-RestMethod "$oldPath/messages" -Method Post -Headers $senderHeaders -ContentType 'application/json' -Body $oldPayload
        if ($oldReceipt.message.seq -ne 1) { throw 'Previous service did not write sequence 1' }
        Stop-Process -Id $appProcess.Id
        $appProcess.WaitForExit(5000) | Out-Null
        if (!$appProcess.HasExited) { throw 'Previous service did not stop' }
        & $binary migrate
        Assert-Exit 'migration from schema 5'
    }
    go test -count=1 -v ./...
    Assert-Exit 'MySQL integration'
    & $binary migrate
    Assert-Exit 'migration replay'
    $app = & $binary create-app harbor dispatch | ConvertFrom-Json
    Assert-Exit 'create demo application'
    if (!$app.admin_token) { throw 'Missing temporary admin token' }
    $appProcess = Start-Process -FilePath $binary -ArgumentList 'serve' -PassThru -WindowStyle Hidden -RedirectStandardError "$fixtureRoot/http.log"
    Wait-Ready $appProcess $origin
    $session = New-FixtureSession 'harbor' $app.admin_token 'operator'
    $me = Invoke-RestMethod "$origin/v1/me" -Headers @{ Authorization = 'Bearer ' + $session.token }
    if (!$me.user_id -or $me.user_id -ne $session.session.user_id -or $me.device_key -ne 'desk') { throw 'Live credential readback mismatch' }
    if ($PreviousBinary) {
        $timeline = Invoke-RestMethod "$origin/v1/admin/conversations/$($oldChat.id)/messages/1/delivery" -Headers @{ Authorization = 'Bearer ' + $oldApp.admin_token }
        if ($timeline.live_delivery -ne $false -or $timeline.seq -ne 1 -or $timeline.dispatched_at) { throw 'Previous message was incorrectly enqueued for live delivery' }
        $replayed = Invoke-RestMethod "$oldPath/messages" -Method Post -Headers $senderHeaders -ContentType 'application/json' -Body $oldPayload
        if (!$replayed.duplicate -or $replayed.message.seq -ne 1) { throw 'Previous idempotency key failed after upgrade' }
        $newPayload = @{ client_id = 'after-upgrade'; body = 'second dispatch' } | ConvertTo-Json
        $newReceipt = Invoke-RestMethod "$oldPath/messages" -Method Post -Headers $senderHeaders -ContentType 'application/json' -Body $newPayload
        $history = Invoke-RestMethod "$oldPath/sync" -Headers $recipientHeaders
        if ($newReceipt.message.seq -ne 2 -or $history.items.Count -ne 2 -or $history.items[0].body -ne 'first dispatch' -or $history.items[1].body -ne 'second dispatch') {
            throw 'The upgraded service did not preserve history and sequence'
        }
        Write-Output 'Schema 5 to 6 preserved the original session, message, idempotency key, and next sequence.'
    }
    Write-Output 'CLI migration, app bootstrap, standalone HTTP readiness, login, and bearer readback passed.'
} finally {
    if ($appProcess -and !$appProcess.HasExited) { Stop-Process -Id $appProcess.Id }
    if ($dbProcess -and !$dbProcess.HasExited) {
        $env:MYSQL_PWD = $adminPassword
        & $mysqladmin @adminArgs shutdown 2>$null
        if (!$dbProcess.WaitForExit(5000)) { Stop-Process -Id $dbProcess.Id }
    }
    if (Test-Path -LiteralPath $bootstrapFile) { Remove-Item -LiteralPath $bootstrapFile }
    foreach ($entry in $environment.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable($entry.Key, $entry.Value, 'Process')
    }
    Pop-Location
}

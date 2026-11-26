param(
    [Parameter(Mandatory)][string]$MySQLBin,
    [string]$PreviousBinary,
    [switch]$VerifyRestore
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

function Stop-FixtureProcess($Process) {
    if ($Process -and !$Process.HasExited) {
        Stop-Process -Id $Process.Id
        if (!$Process.WaitForExit(5000)) { throw 'Fixture process did not stop' }
    }
}

function Assert-FixtureAttachment([string]$Origin, [string]$GrantPath, [hashtable]$Headers, [string]$Destination, [string]$ExpectedHash) {
    $grant = Invoke-RestMethod "$Origin$GrantPath" -Method Post -Headers $Headers
    Invoke-WebRequest "$Origin/v1/attachment-content" -Headers @{ Authorization = 'Attachment ' + $grant.token } -OutFile $Destination
    if ((Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash -ne $ExpectedHash) { throw 'Attachment digest mismatch' }
}

function Test-FixtureRestore($SourceProcess, $Sender, [string]$AdminToken) {
    $peer = $null
    $recovered = $null
    try {
        $peerPort = New-LoopbackPort
        $peerOrigin = "http://127.0.0.1:$peerPort"
        $env:MESSAGING_HTTP_ADDR = "127.0.0.1:$peerPort"
        $env:MESSAGING_NODE_NAME = 'fixture-peer'
        $peer = Start-Process -FilePath $binary -ArgumentList 'serve' -PassThru -WindowStyle Hidden -RedirectStandardError "$fixtureRoot/peer.log"
        Wait-Ready $peer $peerOrigin

        $reader = New-FixtureSession 'harbor' $AdminToken 'receiver'
        $senderHeaders = @{ Authorization = 'Bearer ' + $Sender.token }
        $readerHeaders = @{ Authorization = 'Bearer ' + $reader.token }
        $openChat = @{ user_id = $reader.session.user_id } | ConvertTo-Json
        $chat = Invoke-RestMethod "$origin/v1/conversations/direct" -Method Post -ContentType 'application/json' -Headers $senderHeaders -Body $openChat
        $path = "/v1/conversations/$($chat.id)"
        $bytes = [Text.Encoding]::UTF8.GetBytes('Harbor recovery attachment')
        $digest = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($bytes)).ToLowerInvariant()
        $metadata = @{ filename = 'dispatch.txt'; content_type = 'application/octet-stream'; size = $bytes.Length; sha256 = $digest }
        $attachment = Invoke-RestMethod "$origin/v1/attachments" -Method Post -ContentType 'application/json' -Headers $senderHeaders -Body ($metadata | ConvertTo-Json)
        Invoke-RestMethod "$origin/v1/attachments/$($attachment.id)/content" -Method Put -ContentType 'application/octet-stream' -Headers $senderHeaders -Body $bytes | Out-Null
        $send = @{ client_id = 'restore-sample'; body = 'Original dispatch'; attachment_ids = @($attachment.id) } | ConvertTo-Json
        $receipt = Invoke-RestMethod "$origin$path/messages" -Method Post -ContentType 'application/json' -Headers $senderHeaders -Body $send
        if ($receipt.message.seq -ne 1) { throw 'Expected a new durable message' }
        foreach ($action in @('sync', 'read')) {
            Invoke-RestMethod "$peerOrigin$path/$action" -Method Put -ContentType 'application/json' -Headers $readerHeaders -Body '{"seq":1}' | Out-Null
        }
        $grantPath = "$path/messages/1/attachments/$($attachment.id)/grants"
        Assert-FixtureAttachment $peerOrigin $grantPath $readerHeaders "$fixtureRoot/download-before.bin" $digest

        $group = Invoke-RestMethod "$origin/v1/conversations/group" -Method Post -ContentType 'application/json' -Headers $senderHeaders -Body '{"title":"Recovery shift","history_policy":"since_join"}'
        $groupPath = "/v1/conversations/$($group.id)"
        $batchPath = "$groupPath/membership-batches"
        $selection = @{ client_id = 'restore-roster'; items = @(
            @{ user_id = $reader.session.user_id; action = 'set'; role = 'member' },
            @{ user_id = $Sender.session.user_id; action = 'remove' }
        ) } | ConvertTo-Json -Depth 5
        $batch = Invoke-RestMethod "$origin$batchPath" -Method Post -ContentType 'application/json' -Headers $senderHeaders -Body $selection
        $completed = Invoke-RestMethod "$peerOrigin$batchPath/restore-roster/execute" -Method Post -Headers $senderHeaders
        if ($completed.id -ne $batch.id -or $completed.items[0].status -ne 'succeeded' -or $completed.items[1].reason -ne 'forbidden') { throw 'Expected a partial group roster receipt' }
        $removal = @{ client_id = 'restore-pending'; items = @(@{ user_id = $reader.session.user_id; action = 'remove' }) } | ConvertTo-Json -Depth 5
        $pending = Invoke-RestMethod "$origin$batchPath" -Method Post -ContentType 'application/json' -Headers $senderHeaders -Body $removal
        if ($pending.version -ne 1 -or $pending.items[0].status -ne 'not_started') { throw 'Expected an unstarted group roster receipt' }

        Stop-FixtureProcess $SourceProcess
        Stop-FixtureProcess $peer
        $dump = "$fixtureRoot/messaging.sql"
        $restoreDb = "${dbName}_restore"
        $env:MYSQL_PWD = $adminPassword
        & $mysqldump @adminArgs '--single-transaction' '--hex-blob' '--no-tablespaces' '--set-gtid-purged=OFF' "--result-file=$dump" $dbName
        Assert-Exit 'backup fixture database'
        $restoredBlobs = "$fixtureRoot/restored-attachments"
        Copy-Item -LiteralPath $env:MESSAGING_ATTACHMENT_DIR -Destination $restoredBlobs -Recurse
        & $mysql @adminArgs "--execute=CREATE DATABASE $restoreDb CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci; GRANT ALL PRIVILEGES ON $restoreDb.* TO 'messaging_test'@'127.0.0.1'"
        Assert-Exit 'prepare empty restore database'
        $importLog = "$fixtureRoot/restore-import.log"
        $import = Start-Process -FilePath $mysql -ArgumentList ($adminArgs + $restoreDb) -RedirectStandardInput $dump `
            -RedirectStandardError $importLog -WindowStyle Hidden -Wait -PassThru
        if ($import.ExitCode -ne 0) { throw "MySQL import exited with code $($import.ExitCode). See $importLog" }

        $env:MESSAGING_MYSQL_DSN = "messaging_test:${appPassword}@tcp(127.0.0.1:$dbPort)/$restoreDb"
        $env:MESSAGING_ATTACHMENT_DIR = $restoredBlobs
        $env:MESSAGING_NODE_NAME = 'fixture-restored'
        $recovered = Start-Process -FilePath $binary -ArgumentList 'serve' -PassThru -WindowStyle Hidden -RedirectStandardError "$fixtureRoot/restored.log"
        Wait-Ready $recovered $peerOrigin

        $history = Invoke-RestMethod "$peerOrigin$path/sync?after=0" -Headers $readerHeaders
        $cursor = Invoke-RestMethod "$peerOrigin$path/sync" -Headers $readerHeaders
        $read = Invoke-RestMethod "$peerOrigin$path/read" -Headers $readerHeaders
        if ($history.items.Count -ne 1 -or $history.items[0].client_id -ne 'restore-sample' -or $history.items[0].body -ne 'Original dispatch' -or
            $history.items[0].attachment_ids[0] -ne $attachment.id -or $cursor.items.Count -ne 0 -or $cursor.next_after -ne 1 -or $read.read_seq -ne 1) {
            throw 'Restored message, attachment reference, or device checkpoint did not match'
        }
        Assert-FixtureAttachment $peerOrigin $grantPath $readerHeaders "$fixtureRoot/download-after.bin" $digest
        $continued = Invoke-RestMethod "$peerOrigin$path/messages" -Method Post -ContentType 'application/json' -Headers $senderHeaders -Body '{"client_id":"restored-write","body":"Next dispatch"}'
        if ($continued.message.seq -ne 2) { throw 'Restored conversation did not continue at sequence 2' }
        $saved = Invoke-RestMethod "$peerOrigin$batchPath/restore-roster" -Headers $senderHeaders
        $replay = Invoke-RestMethod "$peerOrigin$batchPath/restore-roster/execute" -Method Post -Headers $senderHeaders
        $unstarted = Invoke-RestMethod "$peerOrigin$batchPath/restore-pending" -Headers $senderHeaders
        if (($saved | ConvertTo-Json -Depth 6 -Compress) -ne ($completed | ConvertTo-Json -Depth 6 -Compress) -or
            ($saved | ConvertTo-Json -Depth 6 -Compress) -ne ($replay | ConvertTo-Json -Depth 6 -Compress) -or
            $unstarted.id -ne $pending.id -or $unstarted.version -ne 1 -or $unstarted.items[0].status -ne 'not_started') {
            throw 'Restored membership operation differs from its committed receipt'
        }
        $resumed = Invoke-RestMethod "$peerOrigin$batchPath/restore-pending/execute" -Method Post -Headers $senderHeaders
        $members = Invoke-RestMethod "$peerOrigin$groupPath/members" -Headers $senderHeaders
        $audit = Invoke-RestMethod "$peerOrigin$groupPath/membership-operations" -Headers $senderHeaders
        if ($resumed.id -ne $pending.id -or $resumed.version -ne 2 -or $resumed.items[0].status -ne 'succeeded' -or
            $members.items.Count -ne 1 -or $members.items[0].user_id -ne $Sender.session.user_id -or $audit.items.Count -ne 2) {
            throw 'Restored group roster produced extra changes or failed to resume'
        }
        Write-Output "Two standalone gateways and isolated restore passed: message seq 1, sync/read seq 1, attachment SHA-256 $digest, next write seq 2, partial and unstarted group batches preserved."
    } finally {
        Stop-FixtureProcess $recovered
        Stop-FixtureProcess $peer
    }
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
$mysqldump = Join-Path $MySQLBin 'mysqldump.exe'
$adminArgs = @('--no-defaults', '--protocol=TCP', '--host=127.0.0.1', "--port=$dbPort", '--user=root')
$serverArgs = @('--no-defaults', '--innodb-buffer-pool-size=32M', '--max-connections=48', '--mysqlx=OFF',
    ('--basedir="{0}"' -f (Split-Path $MySQLBin -Parent)), ('--datadir="{0}/data"' -f $fixtureRoot),
    ('--log-error="{0}/mysql.log"' -f $fixtureRoot))
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
    $dbProcess = Start-Process -FilePath $mysqld -ArgumentList ($serverArgs + @('--bind-address=127.0.0.1', "--port=$dbPort", ('--init-file="{0}"' -f $bootstrapFile))) -PassThru -WindowStyle Hidden
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
        if ($oldVersion.schema_version -notin @(5, 6)) { throw 'Expected a previous binary using schema version 5 or 6' }
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
        $oldGroup = Invoke-RestMethod "$origin/v1/conversations/group" -Method Post -Headers $senderHeaders -ContentType 'application/json' -Body '{"title":"Legacy roster","history_policy":"since_join"}'
        $oldGroupPath = "$origin/v1/conversations/$($oldGroup.id)"
        Invoke-RestMethod "$oldGroupPath/members/$($oldRecipient.session.user_id)" -Method Put -Headers $senderHeaders -ContentType 'application/json' -Body '{"role":"member"}' | Out-Null
        Stop-Process -Id $appProcess.Id
        $appProcess.WaitForExit(5000) | Out-Null
        if (!$appProcess.HasExited) { throw 'Previous service did not stop' }
        $migrated = & $binary migrate | ConvertFrom-Json
        Assert-Exit "migration from schema $($oldVersion.schema_version)"
    }
    if ($env:MESSAGING_CAPACITY_TEST -eq 'full') {
        go test -count=1 -timeout 20m -json ./... | ForEach-Object {
            $event = $_ | ConvertFrom-Json
            if ($event.action -eq 'output') { Write-Output $event.output.TrimEnd() }
        }
    } else {
        go test -count=1 -v ./...
    }
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
        if ($timeline.seq -ne 1 -or ($oldVersion.schema_version -eq 5 -and ($timeline.live_delivery -ne $false -or $timeline.dispatched_at)) -or
            ($oldVersion.schema_version -eq 6 -and $timeline.live_delivery -ne $true)) { throw 'Previous message delivery metadata differs after upgrade' }
        $replayed = Invoke-RestMethod "$oldPath/messages" -Method Post -Headers $senderHeaders -ContentType 'application/json' -Body $oldPayload
        if (!$replayed.duplicate -or $replayed.message.seq -ne 1) { throw 'Previous idempotency key failed after upgrade' }
        $newPayload = @{ client_id = 'after-upgrade'; body = 'second dispatch' } | ConvertTo-Json
        $newReceipt = Invoke-RestMethod "$oldPath/messages" -Method Post -Headers $senderHeaders -ContentType 'application/json' -Body $newPayload
        $history = Invoke-RestMethod "$oldPath/sync" -Headers $recipientHeaders
        if ($newReceipt.message.seq -ne 2 -or $history.items.Count -ne 2 -or $history.items[0].body -ne 'first dispatch' -or $history.items[1].body -ne 'second dispatch') {
            throw 'The upgraded service did not preserve history and sequence'
        }
        $legacyMembers = Invoke-RestMethod "$oldGroupPath/members" -Headers $recipientHeaders
        $legacyAudit = Invoke-RestMethod "$oldGroupPath/membership-operations" -Headers $senderHeaders
        if ($legacyMembers.items.Count -ne 2 -or $legacyAudit.items.Count -ne 0) { throw 'Historical memberships were lost or synthetic audit was produced during upgrade' }
        Invoke-RestMethod "$oldGroupPath/members/$($oldRecipient.session.user_id)" -Method Put -Headers $senderHeaders -ContentType 'application/json' -Body '{"role":"admin"}' | Out-Null
        $newAudit = Invoke-RestMethod "$oldGroupPath/membership-operations" -Headers $senderHeaders
        if ($newAudit.items.Count -ne 1 -or $newAudit.items[0].actor_id -ne $oldSender.session.user_id -or $newAudit.items[0].items[0].role -ne 'admin') {
            throw 'New operation on a historical group was not audited'
        }
        Write-Output "Schema $($oldVersion.schema_version) to $($migrated.schema_version) preserved the original session, message, idempotency key, next sequence, and historical group memberships."
    }
    Write-Output 'CLI migration, app bootstrap, standalone HTTP readiness, login, and bearer readback passed.'
    if ($VerifyRestore) { Test-FixtureRestore $appProcess $session $app.admin_token }
} finally {
    try {
        Stop-FixtureProcess $appProcess
        if ($dbProcess -and !$dbProcess.HasExited) {
            $env:MYSQL_PWD = $adminPassword
            try {
                & $mysqladmin @adminArgs shutdown 2>$null
            } finally {
                if (!$dbProcess.WaitForExit(5000)) { Stop-FixtureProcess $dbProcess }
            }
        }
        if (Test-Path -LiteralPath $bootstrapFile) { Remove-Item -LiteralPath $bootstrapFile }
    } finally {
        foreach ($entry in $environment.GetEnumerator()) {
            [Environment]::SetEnvironmentVariable($entry.Key, $entry.Value, 'Process')
        }
        Pop-Location
    }
}

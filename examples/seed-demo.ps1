param(
    [uri]$Origin = 'http://127.0.0.1:8080'
)

$ErrorActionPreference = 'Stop'
if (!$env:MESSAGING_DEMO_ADMIN_TOKEN) { throw 'Set MESSAGING_DEMO_ADMIN_TOKEN to the demo app admin token' }
$base = $Origin.GetLeftPart([UriPartial]::Authority)
$headers = @{ Authorization = 'Bearer ' + $env:MESSAGING_DEMO_ADMIN_TOKEN }

foreach ($name in @('alice', 'bob', 'carol')) {
    $password = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(18)).ToLowerInvariant()
    $registration = @{ username = $name; display_name = $name; password = $password } | ConvertTo-Json
    $user = Invoke-RestMethod "$base/v1/admin/users" -Method Post -Headers $headers -ContentType 'application/json' -Body $registration
    [pscustomobject]@{ username = $user.username; user_id = $user.user_id; password = $password }
}

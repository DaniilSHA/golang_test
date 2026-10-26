param([string]$BaseURL = 'http://localhost:8080')

$ErrorActionPreference = 'Stop'
$BaseURL = $BaseURL.TrimEnd('/')

function Invoke-API {
    param([string]$Method, [string]$Path, $Body, [string]$Token)
    $parameters = @{ Method = $Method; Uri = "$BaseURL/api/v1$Path"; ContentType = 'application/json' }
    if ($Token) { $parameters.Headers = @{ Authorization = "Bearer $Token" } }
    if ($null -ne $Body) { $parameters.Body = $Body | ConvertTo-Json -Depth 10 -Compress }
    Invoke-RestMethod @parameters
}

function Expect-Status {
    param([int]$Status, [scriptblock]$Action)
    try { & $Action | Out-Null }
    catch {
        if ($null -ne $_.Exception.Response -and [int]$_.Exception.Response.StatusCode -eq $Status) { return }
        throw
    }
    throw "Expected HTTP $Status"
}

$suffix = [Guid]::NewGuid().ToString('N').Substring(0, 12)
$ownerEmail = "owner-$suffix@example.com"
$memberEmail = "member-$suffix@example.com"
$owner = Invoke-API POST /register @{ email = $ownerEmail; password = 'password123'; name = 'Smoke Owner' }
$member = Invoke-API POST /register @{ email = $memberEmail; password = 'password123'; name = 'Smoke Member' }
$ownerToken = (Invoke-API POST /login @{ email = $ownerEmail; password = 'password123' }).access_token
$memberToken = (Invoke-API POST /login @{ email = $memberEmail; password = 'password123' }).access_token
$team = Invoke-API POST /teams @{ name = "Smoke $suffix" } $ownerToken
$foreign = Invoke-API POST /teams @{ name = "Other $suffix" } $memberToken

Expect-Status 404 { Invoke-API GET "/tasks?team_id=$($foreign.id)" $null $ownerToken }
Invoke-API POST "/teams/$($team.id)/invite" @{ email = $memberEmail; role = 'member' } $ownerToken | Out-Null
$task = Invoke-API POST /tasks @{ team_id = $team.id; title = 'Smoke task'; assignee_id = $member.id } $ownerToken
Invoke-API POST "/tasks/$($task.id)/comments" @{ content = 'Working on it' } $memberToken | Out-Null
Expect-Status 403 { Invoke-API PUT "/tasks/$($task.id)" @{ version = 1; title = 'Forbidden' } $memberToken }
$closed = Invoke-API PUT "/tasks/$($task.id)" @{ version = 1; status = 'done' } $memberToken
Expect-Status 409 { Invoke-API PUT "/tasks/$($task.id)" @{ version = 1; title = 'Stale update' } $ownerToken }
$history = Invoke-API GET "/tasks/$($task.id)/history" $null $ownerToken
if ($history.items.Count -ne 2) { throw 'Expected creation and update history' }
$page = Invoke-API GET "/tasks?team_id=$($team.id)&pagination=cursor&limit=1" $null $ownerToken
if ($page.items[0].version -ne 2) { throw 'Task cache returned an old version' }
$stats = Invoke-API GET "/teams/$($team.id)/stats" $null $ownerToken
if ($stats.tasks_by_status.done -ne 1 -or $stats.comments_total -ne 1) { throw 'Incorrect SQL report' }
Expect-Status 409 { Invoke-API DELETE "/teams/$($team.id)/members/$($member.id)" $null $ownerToken }
Invoke-API PUT "/tasks/$($task.id)" @{ version = $closed.version; assignee_id = $null } $ownerToken | Out-Null
Invoke-API DELETE "/teams/$($team.id)/members/$($member.id)" $null $ownerToken | Out-Null
Expect-Status 404 { Invoke-API GET "/tasks?team_id=$($team.id)" $null $memberToken }
Write-Output "Smoke passed. Team=$($team.id), task=$($task.id), owner=$($owner.id)"

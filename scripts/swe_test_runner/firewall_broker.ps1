# Runs from an administrator-protected installation directory. Never executes a client command.
param([Parameter(Mandatory=$true)][string]$PolicyFile)
$ErrorActionPreference = 'Stop'
$policy = Get-Content -LiteralPath $PolicyFile -Raw | ConvertFrom-Json
if ($policy.schema -ne 'agentgo.swe-firewall-session/v2' -or $policy.session_id -notmatch '^[a-f0-9]{32}$') { throw 'invalid_policy' }
$rules = @{}
$leases = @{}
$faults = @{}
$stats = @{}
$events = [Collections.Generic.List[object]]::new()
$fw = New-Object -ComObject HNetCfg.FwPolicy2
$group = 'AgentGo SWE ' + $policy.user_sid + ' ' + $policy.session_id
$stop = $false
$draining = $false
$acknowledged = $false
$armed = $false
$receipt = $null
$guard = $null
$closeReason = 'owner_exit'

function Write-JSON([string]$path, $value) {
    $temporary = Join-Path ([IO.Path]::GetDirectoryName($path)) ('.'+[Guid]::NewGuid().ToString('N')+'.tmp')
    [IO.File]::WriteAllText($temporary,($value | ConvertTo-Json -Depth 10),[Text.UTF8Encoding]::new($false))
    # PowerShell 5.1 coerces $null to an empty string for this .NET argument.
    if ([IO.File]::Exists($path)) { [IO.File]::Replace($temporary,$path,[NullString]::Value) }
    else { [IO.File]::Move($temporary,$path) }
}
function Owner-Alive($record) {
    try {
        $owner=[Diagnostics.Process]::GetProcessById([int]$record.owner_pid)
        return $owner.StartTime.ToUniversalTime().ToFileTimeUtc().ToString() -eq [string]$record.owner_birth
    } catch { return $false }
}
function Remove-Installation([string]$directory) {
    $parent=[IO.Path]::GetFullPath((Join-Path ([Environment]::GetFolderPath('ProgramFiles')) 'AgentGoSWEFirewall'))
    $target=[IO.Path]::GetFullPath($directory)
    if ([IO.Path]::GetDirectoryName($target) -ine $parent -or [IO.Path]::GetFileName($target) -notmatch '^[a-f0-9]{32}$') { throw 'invalid_cleanup_target' }
    if (([IO.File]::GetAttributes($target)-band[IO.FileAttributes]::ReparsePoint)-ne 0) { throw 'reparse_cleanup_target' }
    $files=@(Get-ChildItem -LiteralPath $target -Force)
    foreach ($file in $files) {
        if ($file.PSIsContainer -or ($file.Attributes-band[IO.FileAttributes]::ReparsePoint)-ne 0) { throw 'unknown_installation_entry' }
        if ($file.Name -notin @('broker.ps1','policy.json','session.json','receipt.json') -and $file.Name -notmatch '^\.[a-f0-9]{32}\.tmp$') { throw 'unknown_installation_entry' }
    }
    # Keep ownership metadata until other files are gone, so interrupted cleanup
    # can still be reconciled by the next owner.
    foreach ($file in @($files | Where-Object Name -ne 'policy.json')) { Remove-Item -LiteralPath $file.FullName -Force }
    if ([IO.File]::Exists((Join-Path $target 'policy.json'))) { Remove-Item -LiteralPath (Join-Path $target 'policy.json') -Force }
    Remove-Item -LiteralPath $target -Force
}
function Close-Rules([string]$reason) {
    $failed=[Collections.Generic.List[string]]::new()
    foreach ($entry in @($fw.Rules | Where-Object Grouping -eq $group)) {
        try { $fw.Rules.Remove($entry.Name) } catch { $failed.Add($entry.Name) }
    }
    $remaining=@($fw.Rules | Where-Object Grouping -eq $group).Count
    $acquired=0; $released=0
    foreach ($value in $stats.Values) { $acquired+=$value.acquired; $released+=$value.released }
    $result=@{schema='agentgo.swe-firewall-receipt/v2';session_id=$policy.session_id;closed_at=[DateTime]::UtcNow.ToString('o');reason=$reason;
        complete=($remaining -eq 0 -and $failed.Count -eq 0);remaining=$remaining;failed_rule_names=@($failed);acquired=$acquired;released=$released;
        operation_failures=@($events | Where-Object { -not $_.ok } | Select-Object -Last 32)}
    if ($result.complete) { $rules.Clear(); $leases.Clear() }
    Write-JSON (Join-Path $PSScriptRoot 'receipt.json') $result
    return $result
}
function Recover-Previous {
    $recovered=[Collections.Generic.List[object]]::new()
    $parent=Split-Path -Parent $PSScriptRoot
    foreach ($directory in @(Get-ChildItem -LiteralPath $parent -Directory)) {
        if ($directory.Name -eq $policy.session_id -or $directory.Name -notmatch '^[a-f0-9]{32}$') { continue }
        if (($directory.Attributes-band[IO.FileAttributes]::ReparsePoint)-ne 0) { throw 'reparse_previous_installation' }
        $previousPath=Join-Path $directory.FullName 'policy.json'
        if (-not [IO.File]::Exists($previousPath)) { continue }
        $previous=Get-Content -LiteralPath $previousPath -Raw | ConvertFrom-Json
        if ($previous.schema -ne 'agentgo.swe-firewall-session/v2' -or $previous.user_sid -ne $policy.user_sid) { continue }
        if ($previous.session_id -ne $directory.Name -or (Owner-Alive $previous)) { throw 'previous_owner_still_active' }
        if ($recovered.Count -ge 64) { throw 'recovery_limit' }
        $oldGroup='AgentGo SWE '+$previous.user_sid+' '+$previous.session_id
        $oldReceiptPath=Join-Path $directory.FullName 'receipt.json'
        $prior=$null
        if ([IO.File]::Exists($oldReceiptPath) -and (Get-Item -LiteralPath $oldReceiptPath).Length -lt 8192) {
            $prior=Get-Content -LiteralPath $oldReceiptPath -Raw | ConvertFrom-Json
        }
        $removed=0
        foreach ($entry in @($fw.Rules | Where-Object Grouping -eq $oldGroup)) { $fw.Rules.Remove($entry.Name); $removed++ }
        if (@($fw.Rules | Where-Object Grouping -eq $oldGroup).Count -ne 0) { throw 'previous_rules_not_released' }
        Remove-Installation $directory.FullName
        $recovered.Add(@{session_id=$previous.session_id;removed_rules=$removed;protected_directory_removed=$true;prior_receipt=$prior})
    }
    return ,$recovered.ToArray()
}

function Canonical([string]$path) {
    if ($path -notmatch '^[A-Za-z]:[\\/]' -or $path.Substring(2).Contains(':')) { throw 'invalid_path' }
    $full = [IO.Path]::GetFullPath($path)
    foreach($part in $full.Split('\')) { if($part.EndsWith(' ') -or $part.EndsWith('.')) { throw 'ambiguous_path' } }
    $cursor = $full
    while ($cursor) {
        if ([IO.File]::Exists($cursor) -or [IO.Directory]::Exists($cursor)) {
            if (([IO.File]::GetAttributes($cursor) -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'reparse_path' }
        }
        $parent = [IO.Path]::GetDirectoryName($cursor)
        if ($parent -eq $cursor) { break }
        $cursor = $parent
    }
    return $full
}
function Allowed([string]$program) {
    if (-not [IO.File]::Exists($program) -or [IO.Path]::GetExtension($program) -ine '.exe') { return $false }
    foreach ($exact in $policy.programs) { if ($program -ieq $exact) { return $true } }
    foreach ($root in $policy.roots) { if ($program.StartsWith($root.TrimEnd('\')+'\',[StringComparison]::OrdinalIgnoreCase)) { return $true } }
    return $false
}
function Remove-Lease([string]$id) {
    if (-not $leases.ContainsKey($id)) { return }
    $entry = $leases[$id]
    $leases.Remove($id)
    $stats[$entry.run_id].released += 1
    $r = $rules[$entry.program]
    $r.count -= 1
    if ($r.count -eq 0) { $fw.Rules.Remove($r.name); $rules.Remove($entry.program) }
}
function New-ProgramRule([string]$name,[string]$program) {
    $rule = New-Object -ComObject HNetCfg.FWRule
    $rule.Name=$name; $rule.Grouping=$group; $rule.ApplicationName=$program
    $rule.Direction=1; $rule.Action=1; $rule.Enabled=$true; $rule.Profiles=7
    # Windows rejects IPv6 loopback (::1) as a rule address. Keep the rule
    # narrowly scoped; the real smoke separately verifies IPv6 loopback I/O.
    $rule.Protocol=256; $rule.RemoteAddresses='127.0.0.1'
    $rule.Description='Temporary SWE IPv4 loopback-only inbound permission; owned by '+$policy.session_id
    return $rule
}
function Handle($request) {
    $script:armed=$true
    if ($request.action -eq 'ping') { return @{ok=$true;session_id=$policy.session_id;fault=$faults[[string]$request.run_id];active_leases=$leases.Count;active_rules=$rules.Count;draining=$draining} }
    if ($request.action -eq 'release') { Remove-Lease ([string]$request.lease_id); return @{ok=$true} }
    if ($request.action -eq 'release_run') {
        foreach ($id in @($leases.Keys)) { if ($leases[$id].run_id -eq $request.run_id) { Remove-Lease $id } }
        return @{ok=$true;remaining=@($leases.Values | Where-Object run_id -eq $request.run_id).Count;stats=$stats[[string]$request.run_id]}
    }
    if ($request.action -eq 'shutdown') {
        $script:draining=$true; $script:closeReason='requested'
        $script:receipt=Close-Rules 'requested'
        return @{ok=$true;receipt=$script:receipt}
    }
    if ($request.action -eq 'ack_shutdown') {
        if (-not $draining -or -not $receipt.complete) { throw 'shutdown_not_ready' }
        $script:acknowledged=$true; $script:stop=$true
        return @{ok=$true}
    }
    if ($request.action -eq 'inspect_global') {
        $profiles=@()
        foreach($profile in @(1,2,4)) {
            $profiles+=@{profile=$profile;enabled=$fw.FirewallEnabled($profile);notifications_disabled=$fw.NotificationsDisabled($profile);default_inbound=$fw.DefaultInboundAction($profile)}
        }
        return @{ok=$true;profiles=$profiles;current_profiles=$fw.CurrentProfileTypes}
    }
    if ($request.action -eq 'inspect') {
        $program=Canonical ([string]$request.program)
        if (-not (Allowed $program)) { throw 'program_outside_scope' }
        $items=@($fw.Rules | Where-Object { $_.Grouping -eq $group -and $_.ApplicationName -ieq $program })
        return @{ok=$true;rules=@($items | ForEach-Object {@{name=$_.Name;enabled=$_.Enabled;direction=$_.Direction;action=$_.Action;profiles=$_.Profiles;remote_addresses=$_.RemoteAddresses}})}
    }
    if ($request.action -ne 'acquire') { throw 'unknown_action' }
    if ($draining) { throw 'session_draining' }
    if ($request.run_id -notmatch '^[a-zA-Z0-9_.-]{1,160}$' -or $request.lease_id -notmatch '^[a-f0-9]{32}$') { throw 'invalid_identity' }
    if ($leases.Count -ge 256) { throw 'lease_limit' }
    if (-not $stats.ContainsKey([string]$request.run_id)) {
        if ($stats.Count -ge 2048) { throw 'run_limit' }
        $stats[[string]$request.run_id]=@{acquired=0;released=0}
    }
    $program = Canonical ([string]$request.program)
    if (-not (Allowed $program)) { throw 'program_outside_scope' }
    if ($leases.ContainsKey([string]$request.lease_id)) { throw 'duplicate_lease' }
    if ($fw.LocalPolicyModifyState -ne 0) { throw 'local_firewall_policy_not_effective' }
    if (-not $rules.ContainsKey($program)) {
        foreach ($existing in $fw.Rules) {
            if ($existing.Enabled -and $existing.Direction -eq 1 -and $existing.Action -eq 0 -and $existing.ApplicationName -ieq $program) { throw 'explicit_block_rule' }
        }
        $name = $group + ' ' + [Guid]::NewGuid().ToString('N')
        $rule = New-ProgramRule $name $program
        $fw.Rules.Add($rule)
        $active = $fw.Rules.Item($name)
        if (-not $active.Enabled -or $active.Action -ne 1 -or $active.ApplicationName -ine $program) { throw 'rule_verification_failed' }
        $rules[$program]=@{name=$name;count=0}
    }
    $rules[$program].count += 1
    $leases[[string]$request.lease_id]=@{program=$program;run_id=[string]$request.run_id}
    $stats[[string]$request.run_id].acquired += 1
    return @{ok=$true;lease_id=$request.lease_id;rule_name=$rules[$program].name;program=$program;profiles=7;remote_addresses='127.0.0.1'}
}

try {
    $guard=[Threading.Mutex]::OpenExisting($policy.guard_name)
    if (-not (Owner-Alive $policy) -or [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() -gt $policy.startup_expires) { throw 'startup_expired' }
    $recovered=Recover-Previous
    $security = [IO.Pipes.PipeSecurity]::new()
    $security.AddAccessRule([IO.Pipes.PipeAccessRule]::new([Security.Principal.SecurityIdentifier]::new('S-1-5-2'),[IO.Pipes.PipeAccessRights]::FullControl,[Security.AccessControl.AccessControlType]::Deny))
    foreach ($sid in @($policy.user_sid,'S-1-5-32-544','S-1-5-18')) {
        $security.AddAccessRule([IO.Pipes.PipeAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid),[IO.Pipes.PipeAccessRights]::FullControl,[Security.AccessControl.AccessControlType]::Allow))
    }
    $ready = @{schema='agentgo.swe-firewall-session/v2';session_id=$policy.session_id;pipe=$policy.pipe;roots=$policy.roots;programs=$policy.programs;pid=$PID;ready=$true;recovered_sessions=$recovered}
    Write-JSON (Join-Path $PSScriptRoot 'session.json') $ready
    while (-not $stop) {
        if (Test-Path -LiteralPath $policy.cancel_path) { $closeReason='cancelled'; break }
        try {
            $owner = [Diagnostics.Process]::GetProcessById([int]$policy.owner_pid)
            if ($owner.StartTime.ToUniversalTime().ToFileTimeUtc().ToString() -ne $policy.owner_birth) { break }
        } catch { break }
        $pipe = [IO.Pipes.NamedPipeServerStream]::new($policy.pipe,[IO.Pipes.PipeDirection]::InOut,1,[IO.Pipes.PipeTransmissionMode]::Byte,[IO.Pipes.PipeOptions]::Asynchronous,4096,4096,$security)
        $pending=$null
        try {
            $pending = $pipe.BeginWaitForConnection($null,$null)
            while (-not $pending.AsyncWaitHandle.WaitOne(500)) {
                if ((Test-Path -LiteralPath $policy.cancel_path) -or -not (Owner-Alive $policy) -or (-not $armed -and [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() -gt $policy.startup_expires)) { $stop=$true;break }
            }
            if ($stop) { break }
            $pipe.EndWaitForConnection($pending)
            $buffer = New-Object byte[] 32768
            $count=0; $deadline=[DateTime]::UtcNow.AddSeconds(10)
            while($count -lt $buffer.Length) {
                $read = $pipe.ReadAsync($buffer,$count,$buffer.Length-$count)
                $left=[int][Math]::Max(1,($deadline-[DateTime]::UtcNow).TotalMilliseconds)
                if (-not $read.Wait($left)) { throw 'request_timeout' }
                if ($read.Result -eq 0) { throw 'request_closed' }
                $count += $read.Result
                if ($buffer[$count-1] -eq 10) { break }
            }
            if ($count -eq $buffer.Length) { throw 'request_too_large' }
            $request = [Text.Encoding]::UTF8.GetString($buffer,0,$count).Trim() | ConvertFrom-Json
            try { $response=Handle $request }
            catch {
                $code = if ($_.Exception.Message -match '^[a-z_]+$') { $_.Exception.Message } else { 'firewall_operation_failed' }
                if ($request.run_id -match '^[a-zA-Z0-9_.-]{1,160}$') { $faults[[string]$request.run_id]=$code }
                $response=@{ok=$false;error=$code;diagnostic=@{exception_type=$_.Exception.GetType().FullName;hresult=$_.Exception.HResult;line=$_.InvocationInfo.ScriptLineNumber}}
            }
            if ($request.action -ne 'ping') {
                if ($events.Count -ge 8192) { $events.RemoveAt(0) }
                $events.Add(@{action=$request.action;run_id=$request.run_id;lease_id=$request.lease_id;program=$request.program;rule_name=$response.rule_name;ok=$response.ok;error=$response.error;diagnostic=$response.diagnostic;at=[DateTime]::UtcNow.ToString('o')})
            }
            $bytes = [Text.Encoding]::UTF8.GetBytes(($response | ConvertTo-Json -Compress -Depth 5)+"`n")
            $pipe.Write($bytes,0,$bytes.Length); $pipe.Flush()
        } catch { $closeReason='pipe_failure'; $stop=$true } finally { $pipe.Dispose(); if($pending){$pending.AsyncWaitHandle.Close()} }
    }
} finally {
    try {
        $receipt=Close-Rules $closeReason
        if ($acknowledged -and $receipt.complete) { Remove-Installation $PSScriptRoot }
        # On owner crash, keep only the protected evidence until the next owner
        # exports the recovery result. Never report unverified removal as success.
    } finally { if($guard){$guard.Dispose()} }
}

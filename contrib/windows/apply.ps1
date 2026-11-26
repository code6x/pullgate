# Example apply script. Replace this with the work the approved commit
# requires. The exported files are in $Env:PULLGATE_TREE.
$ErrorActionPreference = 'Stop'
Write-Output "Applied commit $Env:PULLGATE_SHA"
if ($Env:PULLGATE_REPORT_FILE) {
    [System.IO.File]::WriteAllText($Env:PULLGATE_REPORT_FILE, "Applied the approved commit.")
}
exit 0

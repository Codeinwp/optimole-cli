# Installs the optimole CLI on Windows from its GitHub release.
#
#   irm https://raw.githubusercontent.com/Codeinwp/optimole-cli/main/install.ps1 | iex
#
# It downloads the build for this machine, checks it against the release's checksums.txt, puts optimole.exe in
# %LOCALAPPDATA%\Programs\optimole and adds that folder to your user PATH. No administrator rights needed.
# Settings, all optional:
#   $env:OPTIMOLE_VERSION = 'v0.1.0'         a specific release instead of the latest
#   $env:OPTIMOLE_INSTALL_DIR = 'C:\tools'    where to put optimole.exe
#   $env:OPTIMOLE_NO_MODIFY_PATH = '1'        never change PATH
#   $env:OPTIMOLE_DOWNLOAD_URL = '...'        where the release files are (for testing a local build)

& {
    # This block runs in the caller's session: keep its settings to this scope, and restore what it changes.
    Set-StrictMode -Off
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $repo = 'Codeinwp/optimole-cli'
    $version = if ($env:OPTIMOLE_VERSION) { $env:OPTIMOLE_VERSION } else { 'latest' }

    # 32-bit PowerShell on 64-bit Windows reports x86; PROCESSOR_ARCHITEW6432 holds the real CPU.
    $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($cpu) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { throw "optimole install: unsupported CPU $cpu" }
    }
    $archive = "optimole_windows_$arch.zip"
    $dir = if ($env:OPTIMOLE_INSTALL_DIR) { $env:OPTIMOLE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\optimole' }

    $protocols = [Net.ServicePointManager]::SecurityProtocol
    $tmp = $null
    try {
        # Windows PowerShell 5.1 on older .NET may not offer TLS 1.2. Add it only when the session pins explicit
        # protocols (0 is SystemDefault, which already negotiates the best one); restored in finally.
        if ([int]$protocols -ne 0 -and -not ($protocols -band [Net.SecurityProtocolType]::Tls12)) {
            [Net.ServicePointManager]::SecurityProtocol = $protocols -bor [Net.SecurityProtocolType]::Tls12
        }

        $tmp = Join-Path ([IO.Path]::GetTempPath()) ('optimole-' + [guid]::NewGuid())
        [IO.Directory]::CreateDirectory($tmp) | Out-Null

        if ($env:OPTIMOLE_DOWNLOAD_URL) {
            $base = $env:OPTIMOLE_DOWNLOAD_URL.TrimEnd('/')
        } else {
            if ($version -eq 'latest') {
                # Pin the newest release once, so the archive and checksums.txt come from the same one.
                $response = (Invoke-WebRequest -UseBasicParsing -Method Head -Uri "https://github.com/$repo/releases/latest").BaseResponse
                # Windows PowerShell answers an HttpWebResponse (ResponseUri), PowerShell 7 an HttpResponseMessage.
                $final = if ($response.PSObject.Properties['ResponseUri']) { $response.ResponseUri } else { $response.RequestMessage.RequestUri }
                if ("$final" -notmatch '/releases/tag/([^/?#]+)') { throw "optimole install: could not find the latest release of $repo" }
                $version = $Matches[1]
            }
            $base = "https://github.com/$repo/releases/download/$version"
        }

        $archivePath = Join-Path $tmp $archive
        $checksumsPath = Join-Path $tmp 'checksums.txt'
        Write-Host "Downloading $archive ($version)..."
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $archivePath
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $checksumsPath

        $expected = $null
        foreach ($line in [IO.File]::ReadAllLines($checksumsPath)) {
            $fields = $line.Trim() -split '\s+'
            if ($fields.Count -eq 2 -and $fields[1] -eq $archive) { $expected = $fields[0].ToLowerInvariant() }
        }
        if (-not $expected) { throw "optimole install: checksums.txt lists no $archive" }
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
        if ($actual -ne $expected) { throw "optimole install: checksum mismatch for $archive; nothing was installed" }

        $unpacked = Join-Path $tmp 'x'
        Expand-Archive -LiteralPath $archivePath -DestinationPath $unpacked -Force
        [IO.Directory]::CreateDirectory($dir) | Out-Null
        $exe = Join-Path $dir 'optimole.exe'

        # Copy next to the target under a unique name, then swap it in with one file-system call, so an
        # interrupted install never leaves optimole.exe missing or half-written. .NET calls take paths
        # literally (no wildcards in folder names such as "[tools]").
        $staged = Join-Path $dir ('.optimole-' + [guid]::NewGuid() + '.exe')
        try {
            [IO.File]::Copy((Join-Path $unpacked 'optimole.exe'), $staged)
            if ([IO.File]::Exists($exe)) {
                [IO.File]::Replace($staged, $exe, [NullString]::Value)
            } else {
                [IO.File]::Move($staged, $exe)
            }
        } finally {
            if ([IO.File]::Exists($staged)) { [IO.File]::Delete($staged) }
        }

        $installed = & $exe version
        if ($LASTEXITCODE -ne 0) { throw "optimole install: installed $exe, but it does not run" }
        Write-Host "Installed $installed"
        Write-Host "         to $exe"

        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = if ($userPath) { $userPath -split ';' | Where-Object { $_ } } else { @() }
        if ($entries -notcontains $dir) {
            if ($env:OPTIMOLE_NO_MODIFY_PATH -eq '1') {
                Write-Host "`n$dir is not on your PATH; add it to use optimole from any terminal."
            } else {
                [Environment]::SetEnvironmentVariable('Path', (@($entries) + $dir) -join ';', 'User')
                $env:Path = "$env:Path;$dir"
                Write-Host "`nAdded $dir to your PATH. New terminals pick it up; this one already has it."
            }
        }
        Write-Host "`nNext: optimole login   (paste an API key from the Optimole dashboard)"
    } finally {
        if ($tmp -and [IO.Directory]::Exists($tmp)) { [IO.Directory]::Delete($tmp, $true) }
        [Net.ServicePointManager]::SecurityProtocol = $protocols
    }
}

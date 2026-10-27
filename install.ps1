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
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $repo = 'Codeinwp/optimole-cli'
    $version = if ($env:OPTIMOLE_VERSION) { $env:OPTIMOLE_VERSION } else { 'latest' }
    if ($env:OPTIMOLE_DOWNLOAD_URL) {
        $base = $env:OPTIMOLE_DOWNLOAD_URL.TrimEnd('/')
    } elseif ($version -eq 'latest') {
        $base = "https://github.com/$repo/releases/latest/download"
    } else {
        $base = "https://github.com/$repo/releases/download/$version"
    }

    # 32-bit PowerShell on 64-bit Windows reports x86; PROCESSOR_ARCHITEW6432 holds the real CPU.
    $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($cpu) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { throw "optimole install: unsupported CPU $cpu" }
    }
    $archive = "optimole_windows_$arch.zip"
    $dir = if ($env:OPTIMOLE_INSTALL_DIR) { $env:OPTIMOLE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\optimole' }

    # Windows PowerShell 5.1 may not offer TLS 1.2 by default.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('optimole-' + [guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "Downloading $archive ($version)..."
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

        $expected = $null
        foreach ($line in Get-Content (Join-Path $tmp 'checksums.txt')) {
            $fields = $line.Trim() -split '\s+'
            if ($fields.Count -eq 2 -and $fields[1] -eq $archive) { $expected = $fields[0].ToLowerInvariant() }
        }
        if (-not $expected) { throw "optimole install: checksums.txt lists no $archive" }
        $actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash.ToLowerInvariant()
        if ($actual -ne $expected) { throw "optimole install: checksum mismatch for $archive; nothing was installed" }

        Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x') -Force
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $exe = Join-Path $dir 'optimole.exe'
        Copy-Item -Path (Join-Path $tmp 'x\optimole.exe') -Destination $exe -Force

        Write-Host ("Installed " + (& $exe version))
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
        Remove-Item -Recurse -Force -Path $tmp -ErrorAction SilentlyContinue
    }
}

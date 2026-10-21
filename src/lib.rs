//! Zed extension adapter for launching the Go CodeLens language server.

use sha2::{Digest, Sha256};
use std::{
    fs,
    io::{self, Read},
    path::Path,
};
use zed_extension_api::{self as zed, Result};

const GITHUB_REPOSITORY: &str = "johnny0x38e/go-codelens-for-zed";
const SERVER_NAME: &str = "go-codelens-ls";
const SERVER_VERSION: &str = env!("CARGO_PKG_VERSION");
const CHECKSUMS_ASSET: &str = "SHA256SUMS";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct ReleaseTarget {
    triple: &'static str,
    executable_suffix: &'static str,
    needs_executable_bit: bool,
}

impl ReleaseTarget {
    fn asset_name(self) -> String {
        format!(
            "{SERVER_NAME}-v{SERVER_VERSION}-{}{}",
            self.triple, self.executable_suffix
        )
    }

    fn binary_path(self) -> String {
        format!(
            "{}/{SERVER_NAME}{}",
            installation_directory(),
            self.executable_suffix
        )
    }
}

struct GoCodeLensExtension {
    cached_binary_path: Option<String>,
}

impl GoCodeLensExtension {
    fn release_target() -> Result<ReleaseTarget> {
        release_target(zed::current_platform())
    }

    fn downloaded_server_path(
        &mut self,
        language_server_id: &zed::LanguageServerId,
        target: ReleaseTarget,
    ) -> Result<String> {
        if let Some(path) = self.cached_binary_path.as_ref()
            && Path::new(path).is_file()
        {
            return Ok(path.clone());
        }

        let binary_path = target.binary_path();
        if Path::new(&binary_path).is_file() {
            self.cached_binary_path = Some(binary_path.clone());
            return Ok(binary_path);
        }

        zed::set_language_server_installation_status(
            language_server_id,
            &zed::LanguageServerInstallationStatus::CheckingForUpdate,
        );
        match install_server(language_server_id, target, &binary_path) {
            Ok(()) => {
                self.cached_binary_path = Some(binary_path.clone());
                zed::set_language_server_installation_status(
                    language_server_id,
                    &zed::LanguageServerInstallationStatus::None,
                );
                Ok(binary_path)
            }
            Err(error) => {
                zed::set_language_server_installation_status(
                    language_server_id,
                    &zed::LanguageServerInstallationStatus::Failed(error.clone()),
                );
                Err(error)
            }
        }
    }
}

impl zed::Extension for GoCodeLensExtension {
    fn new() -> Self {
        Self {
            cached_binary_path: None,
        }
    }

    fn language_server_command(
        &mut self,
        language_server_id: &zed::LanguageServerId,
        worktree: &zed::Worktree,
    ) -> Result<zed::Command> {
        let target = Self::release_target()?;

        // PATH remains the development override. Marketplace installations use
        // the version-pinned, checksummed binary managed below.
        let command = if let Some(path) = worktree.which(SERVER_NAME) {
            zed::set_language_server_installation_status(
                language_server_id,
                &zed::LanguageServerInstallationStatus::None,
            );
            path
        } else {
            self.downloaded_server_path(language_server_id, target)?
        };

        Ok(zed::Command {
            command,
            args: Vec::new(),
            env: Vec::new(),
        })
    }
}

fn release_target(platform: (zed::Os, zed::Architecture)) -> Result<ReleaseTarget> {
    let target = match platform {
        (zed::Os::Mac, zed::Architecture::Aarch64) => ReleaseTarget {
            triple: "aarch64-apple-darwin",
            executable_suffix: "",
            needs_executable_bit: true,
        },
        (zed::Os::Mac, zed::Architecture::X8664) => ReleaseTarget {
            triple: "x86_64-apple-darwin",
            executable_suffix: "",
            needs_executable_bit: true,
        },
        (zed::Os::Linux, zed::Architecture::Aarch64) => ReleaseTarget {
            triple: "aarch64-unknown-linux-gnu",
            executable_suffix: "",
            needs_executable_bit: true,
        },
        (zed::Os::Linux, zed::Architecture::X8664) => ReleaseTarget {
            triple: "x86_64-unknown-linux-gnu",
            executable_suffix: "",
            needs_executable_bit: true,
        },
        (zed::Os::Windows, zed::Architecture::Aarch64) => ReleaseTarget {
            triple: "aarch64-pc-windows-msvc",
            executable_suffix: ".exe",
            needs_executable_bit: false,
        },
        (zed::Os::Windows, zed::Architecture::X8664) => ReleaseTarget {
            triple: "x86_64-pc-windows-msvc",
            executable_suffix: ".exe",
            needs_executable_bit: false,
        },
        (os, architecture) => {
            return Err(format!(
                "Go CodeLens supports macOS, Linux, and Windows on arm64 or x86_64; got {os:?}/{architecture:?}"
            ));
        }
    };
    Ok(target)
}

fn installation_directory() -> String {
    format!("{SERVER_NAME}-v{SERVER_VERSION}")
}

fn install_server(
    language_server_id: &zed::LanguageServerId,
    target: ReleaseTarget,
    binary_path: &str,
) -> Result<()> {
    let release_tag = format!("v{SERVER_VERSION}");
    let release =
        zed::github_release_by_tag_name(GITHUB_REPOSITORY, &release_tag).map_err(|error| {
            format!("failed to find Go CodeLens server release {release_tag}: {error}")
        })?;
    let binary_asset_name = target.asset_name();
    let binary_url = release_asset_url(&release, &binary_asset_name)?;
    let checksums_url = release_asset_url(&release, CHECKSUMS_ASSET)?;

    let installation_directory = installation_directory();
    fs::create_dir_all(&installation_directory)
        .map_err(|error| format!("failed to create server installation directory: {error}"))?;

    zed::set_language_server_installation_status(
        language_server_id,
        &zed::LanguageServerInstallationStatus::Downloading,
    );

    let checksums_path = format!("{installation_directory}/{CHECKSUMS_ASSET}");
    remove_file_if_present(&checksums_path)?;
    zed::download_file(
        &checksums_url,
        &checksums_path,
        zed::DownloadedFileType::Uncompressed,
    )
    .map_err(|error| format!("failed to download {CHECKSUMS_ASSET}: {error}"))?;
    let checksums = fs::read_to_string(&checksums_path)
        .map_err(|error| format!("failed to read {CHECKSUMS_ASSET}: {error}"))?;
    let expected_checksum = checksum_for_asset(&checksums, &binary_asset_name)?;

    let temporary_path = format!("{binary_path}.download");
    remove_file_if_present(&temporary_path)?;
    zed::download_file(
        &binary_url,
        &temporary_path,
        zed::DownloadedFileType::Uncompressed,
    )
    .map_err(|error| format!("failed to download {binary_asset_name}: {error}"))?;

    let actual_checksum = sha256_file(&temporary_path)?;
    if !actual_checksum.eq_ignore_ascii_case(&expected_checksum) {
        remove_file_if_present(&temporary_path)?;
        return Err(format!(
            "checksum mismatch for {binary_asset_name}: expected {expected_checksum}, got {actual_checksum}"
        ));
    }
    if target.needs_executable_bit {
        zed::make_file_executable(&temporary_path).map_err(|error| {
            format!("failed to make downloaded Go CodeLens server executable: {error}")
        })?;
    }

    fs::rename(&temporary_path, binary_path)
        .map_err(|error| format!("failed to publish downloaded Go CodeLens server: {error}"))?;
    Ok(())
}

fn release_asset_url(release: &zed::GithubRelease, asset_name: &str) -> Result<String> {
    release
        .assets
        .iter()
        .find(|asset| asset.name == asset_name)
        .map(|asset| asset.download_url.clone())
        .ok_or_else(|| {
            format!("release v{SERVER_VERSION} does not contain required asset {asset_name}")
        })
}

fn checksum_for_asset(checksums: &str, asset_name: &str) -> Result<String> {
    let mut match_checksum = None;
    for line in checksums.lines() {
        let mut fields = line.split_whitespace();
        let Some(checksum) = fields.next() else {
            continue;
        };
        let Some(file_name) = fields.next() else {
            continue;
        };
        if file_name.trim_start_matches('*') != asset_name {
            continue;
        }
        if fields.next().is_some()
            || checksum.len() != 64
            || !checksum.bytes().all(|byte| byte.is_ascii_hexdigit())
        {
            return Err(format!(
                "{CHECKSUMS_ASSET} contains an invalid entry for {asset_name}"
            ));
        }
        if match_checksum.is_some() {
            return Err(format!(
                "{CHECKSUMS_ASSET} contains duplicate entries for {asset_name}"
            ));
        }
        match_checksum = Some(checksum.to_ascii_lowercase());
    }

    match_checksum
        .ok_or_else(|| format!("{CHECKSUMS_ASSET} does not contain a checksum for {asset_name}"))
}

fn sha256_file(path: &str) -> Result<String> {
    let file = fs::File::open(path)
        .map_err(|error| format!("failed to open downloaded server for verification: {error}"))?;
    sha256_reader(file)
        .map_err(|error| format!("failed to verify downloaded server checksum: {error}"))
}

fn sha256_reader(mut reader: impl Read) -> io::Result<String> {
    let mut digest = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let bytes_read = reader.read(&mut buffer)?;
        if bytes_read == 0 {
            break;
        }
        digest.update(&buffer[..bytes_read]);
    }
    Ok(format!("{:x}", digest.finalize()))
}

fn remove_file_if_present(path: &str) -> Result<()> {
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(format!("failed to remove stale download {path}: {error}")),
    }
}

zed::register_extension!(GoCodeLensExtension);

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    #[test]
    fn maps_all_supported_release_targets() {
        let cases = [
            (
                (zed::Os::Mac, zed::Architecture::Aarch64),
                "aarch64-apple-darwin",
                "",
            ),
            (
                (zed::Os::Mac, zed::Architecture::X8664),
                "x86_64-apple-darwin",
                "",
            ),
            (
                (zed::Os::Linux, zed::Architecture::Aarch64),
                "aarch64-unknown-linux-gnu",
                "",
            ),
            (
                (zed::Os::Linux, zed::Architecture::X8664),
                "x86_64-unknown-linux-gnu",
                "",
            ),
            (
                (zed::Os::Windows, zed::Architecture::Aarch64),
                "aarch64-pc-windows-msvc",
                ".exe",
            ),
            (
                (zed::Os::Windows, zed::Architecture::X8664),
                "x86_64-pc-windows-msvc",
                ".exe",
            ),
        ];

        for (platform, expected_triple, expected_suffix) in cases {
            let target = release_target(platform).expect("supported target");
            assert_eq!(target.triple, expected_triple);
            assert_eq!(target.executable_suffix, expected_suffix);
            assert_eq!(
                target.asset_name(),
                format!("{SERVER_NAME}-v{SERVER_VERSION}-{expected_triple}{expected_suffix}")
            );
        }
    }

    #[test]
    fn rejects_unsupported_architecture() {
        let error = release_target((zed::Os::Linux, zed::Architecture::X86))
            .expect_err("x86 must remain unsupported");
        assert!(error.contains("arm64 or x86_64"));
    }

    #[test]
    fn extracts_exact_checksum_entry() {
        let asset = format!("{SERVER_NAME}-v{SERVER_VERSION}-aarch64-apple-darwin");
        let checksum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
        let contents = format!("{checksum}  unrelated\n{checksum}  {asset}\n");

        assert_eq!(checksum_for_asset(&contents, &asset).unwrap(), checksum);
        assert!(checksum_for_asset(&contents, "missing").is_err());
    }

    #[test]
    fn rejects_duplicate_or_malformed_checksum_entries() {
        let checksum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";
        let duplicate = format!("{checksum}  server\n{checksum}  server\n");

        assert!(checksum_for_asset(&duplicate, "server").is_err());
        assert!(checksum_for_asset("not-a-sha256  server\n", "server").is_err());
    }

    #[test]
    fn computes_sha256_without_loading_the_whole_binary() {
        let checksum = sha256_reader(Cursor::new(b"abc")).unwrap();
        assert_eq!(
            checksum,
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }
}

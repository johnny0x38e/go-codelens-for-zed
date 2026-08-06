use std::{fs, path::PathBuf};

use zed_extension_api::{self as zed, Result};

struct GoImplementationLensExtension;

impl GoImplementationLensExtension {
    fn binary_name() -> Result<&'static str> {
        match zed::current_platform() {
            (zed::Os::Mac, zed::Architecture::Aarch64) => {
                Ok("go-implementation-lens-ls-aarch64-apple-darwin")
            }
            (os, architecture) => Err(format!(
                "Go Implementation Lens currently supports macOS on Apple Silicon only; got {os:?}/{architecture:?}"
            )),
        }
    }

    fn binary_path() -> Result<PathBuf> {
        let path = std::env::current_dir()
            .map_err(|error| format!("failed to locate the extension directory: {error}"))?
            .join("bin")
            .join(Self::binary_name()?);

        if !fs::metadata(&path).is_ok_and(|metadata| metadata.is_file()) {
            return Err(format!(
                "language server binary is missing at {}. Run ./scripts/build-server.sh before installing the dev extension",
                path.display()
            ));
        }

        Ok(path)
    }
}

impl zed::Extension for GoImplementationLensExtension {
    fn new() -> Self {
        Self
    }

    fn language_server_command(
        &mut self,
        _language_server_id: &zed::LanguageServerId,
        _worktree: &zed::Worktree,
    ) -> Result<zed::Command> {
        let binary = Self::binary_path()?;

        Ok(zed::Command {
            command: binary.to_string_lossy().into_owned(),
            args: Vec::new(),
            env: Vec::new(),
        })
    }
}

zed::register_extension!(GoImplementationLensExtension);

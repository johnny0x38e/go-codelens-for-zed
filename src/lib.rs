use zed_extension_api::{self as zed, Result};

struct GoImplementationLensExtension;

impl GoImplementationLensExtension {
    fn ensure_supported_platform() -> Result<()> {
        match zed::current_platform() {
            (zed::Os::Mac, zed::Architecture::Aarch64) => Ok(()),
            (os, architecture) => Err(format!(
                "Go Implementation Lens currently supports macOS on Apple Silicon only; got {os:?}/{architecture:?}"
            )),
        }
    }
}

impl zed::Extension for GoImplementationLensExtension {
    fn new() -> Self {
        Self
    }

    fn language_server_command(
        &mut self,
        _language_server_id: &zed::LanguageServerId,
        worktree: &zed::Worktree,
    ) -> Result<zed::Command> {
        Self::ensure_supported_platform()?;

        let command = worktree
            .which("go-implementation-lens-ls")
            .ok_or_else(|| {
                "go-implementation-lens-ls was not found on Zed's workspace PATH. Run ./scripts/install-server.sh, then restart the language server"
                    .to_owned()
            })?;

        Ok(zed::Command {
            command,
            args: Vec::new(),
            env: Vec::new(),
        })
    }
}

zed::register_extension!(GoImplementationLensExtension);

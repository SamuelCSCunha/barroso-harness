package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Confirmar decides whether the update proceeds. It exists so the REPL can
// ask and tests can pin the answer. Nil = proceed without asking (-p mode,
// tests).
type Confirmar func() bool

// Rodar checks the channel, downloads the newest release, verifies and
// installs it. It is the only entry point the CLI calls.
//
// Confirmation comes BEFORE the download: there is no reason to download
// 11 MB only to find out the person did not want to replace the binary.
//
// Possible errors, and what each means:
//   - missing token           → private channel without credentials; nothing done
//   - channel rejected token  → permissions; nothing done
//   - no stable release       → empty channel for this platform; nothing done
//   - checksum mismatch       → corrupted download; staged file removed, binary intact
//   - binary in use           → Windows cannot rename a running executable; exits
//     with the PowerShell command for the person to run
//
// In every failure BEFORE installation, the running executable is left
// untouched. A failed update never leaves the machine without a harness.
func Rodar(ctx context.Context, out io.Writer, currentVersion string, confirm Confirmar) error {
	rel, err := BuscarUltima(ctx)
	if err != nil {
		return fmt.Errorf("update channel unavailable: %w", err)
	}

	cmp, err := compareVersions(currentVersion, rel.Version)
	if err != nil {
		return fmt.Errorf("compare versions: %w", err)
	}
	switch cmp {
	case 0:
		fmt.Fprintf(out, "already on the latest version (%s).\n", currentVersion)
		return nil
	case 1:
		fmt.Fprintf(out, "installed version (%s) is newer than the published one (%s); nothing to do.\n",
			currentVersion, rel.Version)
		return nil
	}

	asset, err := rel.AssetDaPlataforma()
	if err != nil {
		return err
	}

	// The checksum comes from the SHA256SUMS published with the release.
	// Without it, the integrity check has no reference — and downloading a
	// binary without checking is exactly the hole this package closes.
	sum := ""
	if sumsAsset, ok := rel.AssetSomas(); ok {
		sums, errSums := ListarSomas(ctx, sumsAsset)
		if errSums != nil {
			return fmt.Errorf("could not read the release SHA256SUMS (%v); aborting for safety", errSums)
		}
		sum = sums[asset.Name]
		if sum == "" {
			return fmt.Errorf("SHA256SUMS of release %s does not list %s; aborting for safety",
				rel.Version, asset.Name)
		}
	} else {
		return errors.New("release did not publish SHA256SUMS; aborting for safety")
	}

	if confirm != nil && !confirm() {
		fmt.Fprintln(out, "update canceled; current installation kept.")
		return nil
	}

	staging := filepath.Join(stagingRoot(), asset.Name)
	defer removeIfExists(staging)

	fmt.Fprintf(out, "downloading %s (%s, ~%d MiB)...\n", asset.Name, rel.Version, asset.Bytes/(1<<20))
	if _, err := Baixar(ctx, asset, sum, staging); err != nil {
		return err
	}
	if err := verifyStaged(staging, rel.Version); err != nil {
		return fmt.Errorf("new binary failed the exec check: %w", err)
	}

	result, err := installBinary(staging)
	if err != nil {
		if len(result.commands) > 0 {
			fmt.Fprintln(out, "not enough permission to replace the binary; run the following, then retry /update:")
			fmt.Fprintln(out, installCommands(result))
			return err
		}
		return err
	}
	fmt.Fprintf(out, "installed %s; exit and reopen bh to use the new version.\n", rel.Version)
	return nil
}

// stagingRoot is where the downloaded binary waits for installation. It lives
// in the user cache — never next to the executable, where a temp file with
// the exec bit could be mistaken for a half-done installation.
func stagingRoot() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		// With no known cache, the executable's own dir accepts a file with
		// an unmistakable name; the installation removes it in the defer.
		if exe, errExe := os.Executable(); errExe == nil {
			return filepath.Dir(exe)
		}
		return "."
	}
	return filepath.Join(dir, "barroso-harness", "updates")
}

// normalizeVersion cleans up "dev" — a local build without a release
// version. In that case any published version is new.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "dev"
	}
	return v
}

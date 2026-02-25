package appbundle

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Options struct {
	AppName   string
	OutputDir string
	UIListen  string
}

func Create(opts Options) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("-make-macos-app is only supported on macOS")
	}
	if strings.TrimSpace(opts.AppName) == "" {
		opts.AppName = "Windows-Packager"
	}
	if strings.TrimSpace(opts.OutputDir) == "" {
		opts.OutputDir = "."
	}
	if strings.TrimSpace(opts.UIListen) == "" {
		opts.UIListen = "127.0.0.1:8080"
	}

	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}

	appPath := filepath.Join(opts.OutputDir, opts.AppName+".app")
	contents := filepath.Join(appPath, "Contents")
	macosDir := filepath.Join(contents, "MacOS")

	if err := os.RemoveAll(appPath); err != nil {
		return "", fmt.Errorf("remove existing app bundle: %w", err)
	}
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		return "", fmt.Errorf("create app bundle dirs: %w", err)
	}

	launcherPath := filepath.Join(macosDir, opts.AppName)
	binPath := filepath.Join(macosDir, "windows-packager-bin")
	plistPath := filepath.Join(contents, "Info.plist")

	if err := copyFile(exePath, binPath, 0o755); err != nil {
		return "", fmt.Errorf("copy binary: %w", err)
	}
	if err := os.WriteFile(launcherPath, []byte(launcherScript(opts.UIListen)), 0o755); err != nil {
		return "", fmt.Errorf("write launcher script: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(infoPlist(opts.AppName)), 0o644); err != nil {
		return "", fmt.Errorf("write Info.plist: %w", err)
	}

	return appPath, nil
}

func launcherScript(uiListen string) string {
	return "#!/bin/zsh\n" +
		"set -euo pipefail\n" +
		"APP_DIR=\"$(cd \"$(dirname \"$0\")\" && pwd)\"\n" +
		"BIN=\"$APP_DIR/windows-packager-bin\"\n" +
		"LISTEN=\"${WINDOWS_PACKAGER_UI_LISTEN:-" + uiListen + "}\"\n" +
		"LOG=\"$HOME/Library/Logs/Windows-Packager.log\"\n" +
		"\"$BIN\" -ui -ui-listen \"$LISTEN\" >> \"$LOG\" 2>&1 &\n" +
		"sleep 1\n" +
		"open \"http://$LISTEN\"\n"
}

func infoPlist(appName string) string {
	bundleID := "com.mancalazure." + strings.ToLower(strings.ReplaceAll(appName, " ", "-"))
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>
  <string>` + appName + `</string>
  <key>CFBundleDisplayName</key>
  <string>` + appName + `</string>
  <key>CFBundleExecutable</key>
  <string>` + appName + `</string>
  <key>CFBundleIdentifier</key>
  <string>` + bundleID + `</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>1.0.0</string>
  <key>CFBundleVersion</key>
  <string>1</string>
  <key>LSMinimumSystemVersion</key>
  <string>12.0</string>
  <key>NSHighResolutionCapable</key>
  <true/>
</dict>
</plist>
`
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

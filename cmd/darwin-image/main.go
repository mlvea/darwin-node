package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/darwin-node/darwin-node/pkg/image"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "darwin-image",
		Short: "Bake, inject, pack, and verify darwin-node macOS VM images",
	}
	root.AddCommand(cmdVerify(), cmdPack(), cmdInject(), cmdRestore(), cmdPull(), cmdDeltaCreate(), cmdDeltaApply())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func cmdVerify() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <dir-or-config.json>",
		Short: "Parse and validate an image config (Darwin-Node or Agoda)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := args[0]
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				img, err := image.LoadDir(p)
				if err != nil {
					return err
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(img.Config)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			c, err := image.ParseConfig(b)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(c)
		},
	}
}

func cmdPack() *cobra.Command {
	var hw, mid string
	c := &cobra.Command{
		Use:   "pack <image-dir>",
		Short: "Write config.json for a directory that already has disk.img and aux.img",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := image.Config{OS: "darwin", HardwareModelData: hw, MachineIdData: mid}
			return image.PackDir(args[0], cfg)
		},
	}
	c.Flags().StringVar(&hw, "hardware-model", "", "base64 hardwareModelData")
	c.Flags().StringVar(&mid, "machine-id", "", "base64 machineIdData (template; runtime mints a unique ID)")
	return c
}

func cmdPull() *cobra.Command {
	var cache string
	c := &cobra.Command{
		Use:   "pull <ref>",
		Short: "Pull a Darwin-Node / Agoda OCI VM image into the local cache",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cache == "" {
				home, _ := os.UserHomeDir()
				cache = filepath.Join(home, "Library", "Caches", "io.darwin-node.node")
			}
			img, err := image.NewManager(cache).Pull(context.Background(), args[0], image.RegistryCreds{}, false)
			if err != nil {
				return err
			}
			fmt.Println(img.Dir)
			return nil
		},
	}
	c.Flags().StringVar(&cache, "cache", "", "cache root")
	return c
}

func cmdInject() *cobra.Command {
	var imageDir, agentBin, outPlist string
	c := &cobra.Command{
		Use:   "inject-agent",
		Short: "Copy guest agent + launchd plist into a mounted image tree",
		RunE: func(cmd *cobra.Command, args []string) error {
			if imageDir == "" || agentBin == "" {
				return fmt.Errorf("--image and --agent are required")
			}
			dest := filepath.Join(imageDir, "usr", "local", "bin", "darwin-guest-agent")
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			in, err := os.ReadFile(agentBin)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dest, in, 0o755); err != nil {
				return err
			}
			if outPlist == "" {
				outPlist = filepath.Join(imageDir, "Library", "LaunchDaemons", "io.darwin-node.guest-agent.plist")
			}
			if err := os.MkdirAll(filepath.Dir(outPlist), 0o755); err != nil {
				return err
			}
			return os.WriteFile(outPlist, []byte(guestLaunchdPlist), 0o644)
		},
	}
	c.Flags().StringVar(&imageDir, "image", "", "mounted guest filesystem root")
	c.Flags().StringVar(&agentBin, "agent", "", "path to darwin-guest-agent binary")
	c.Flags().StringVar(&outPlist, "plist", "", "override launchd plist destination")
	return c
}

func cmdRestore() *cobra.Command {
	c := &cobra.Command{
		Use:   "restore",
		Short: "Restore a macOS IPSW into a VM disk (Apple Silicon + Virtualization.framework)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return restoreIPSW(cmd)
		},
	}
	c.Flags().String("ipsw", "", "path to UniversalMac IPSW")
	c.Flags().String("out", "", "output image directory")
	return c
}

const guestLaunchdPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>io.darwin-node.guest-agent</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/bin/darwin-guest-agent</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/var/log/darwin-guest-agent.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/darwin-guest-agent.log</string>
</dict>
</plist>
`

func cmdDeltaCreate() *cobra.Command {
	var base, target, outDir, name string
	c := &cobra.Command{
		Use:   "delta-create --base <image-dir> --target <image-dir> --out <delta-dir>",
		Short: "Write a verifiable delta that turns the base image into the target",
		RunE: func(cmd *cobra.Command, args []string) error {
			man, err := image.ReadDeltaManifest(outDir)
			if err != nil {
				man = image.DeltaManifest{}
			}
			ref, err := image.CreatePatch(
				filepath.Join(base, "disk.img"),
				filepath.Join(target, "disk.img"),
				outDir, name)
			if err != nil {
				return err
			}
			man.Patches = append(man.Patches, ref)
			if err := image.WriteDeltaManifest(outDir, man); err != nil {
				return err
			}
			fmt.Printf("wrote %s (%s -> %s)\n", filepath.Join(outDir, ref.Name+".patch"), shortSHA(ref.BaseSHA), shortSHA(ref.DestSHA))
			return nil
		},
	}
	c.Flags().StringVar(&base, "base", "", "base image dir (must be present on every applying host)")
	c.Flags().StringVar(&target, "target", "", "newly baked image dir")
	c.Flags().StringVar(&outDir, "out", "", "delta output dir")
	c.Flags().StringVar(&name, "name", "disk.img", "file inside the image dir this patch targets")
	_ = c.MarkFlagRequired("base")
	_ = c.MarkFlagRequired("target")
	_ = c.MarkFlagRequired("out")
	return c
}

func cmdDeltaApply() *cobra.Command {
	var baseDir, deltaDir, out string
	c := &cobra.Command{
		Use:   "delta-apply --base <image-dir> --delta <delta-dir> --out <image-dir>",
		Short: "Apply a delta to a verified base and produce a full image dir",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := image.ApplyDelta(baseDir, deltaDir, out); err != nil {
				return err
			}
			man, err := image.ReadDeltaManifest(deltaDir)
			if err != nil {
				return err
			}
			img, err := image.LoadDir(out)
			if err != nil {
				return err
			}
			fmt.Printf("applied %d patches -> %s (disk %s)\n", len(man.Patches), out, img.DiskPath)
			return nil
		},
	}
	c.Flags().StringVar(&baseDir, "base", "", "verified base image dir")
	c.Flags().StringVar(&deltaDir, "delta", "", "delta dir containing delta.json")
	c.Flags().StringVar(&out, "out", "", "destination image dir (must not exist)")
	_ = c.MarkFlagRequired("base")
	_ = c.MarkFlagRequired("delta")
	_ = c.MarkFlagRequired("out")
	return c
}

func shortSHA(sha string) string {
	const prefix = "sha256-"
	if len(sha) >= len(prefix)+12 {
		return sha[len(prefix) : len(prefix)+12]
	}
	return sha
}

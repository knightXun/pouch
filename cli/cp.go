package main

import (
	"context"
	"fmt"
	"os"
	"io"
	"strings"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/alibaba/pouch/apis/types"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/pkg/archive"
)

// createDescription is used to describe create command in detail and auto generate command doc.
var copyDescription = "Copy files/folders between a container and the local filesystem\n" +
	"\nUse '-' as the source to read a tar archive from stdin\n" +
	"and extract it to a directory destination in a container.\n" +
	"Use '-' as the destination to stream a tar archive of a\n" +
	"container source to stdout."

// CreateCommand use to implement 'create' command, it create a container.
type CopyCommand struct {
	*container
	baseCommand
}

type copyOptions struct {
	source      string
	destination string
	followLink  bool
}

type cpConfig struct {
	followLink bool
}

// Init initialize create command.
func (cc *CopyCommand) Init(c *Cli) {
	var opts copyOptions

	cc.cli = c
	cc.cmd = &cobra.Command{
		Use:   `cp [OPTIONS] CONTAINER:SRC_PATH DEST_PATH|-
	pouch cp [OPTIONS] SRC_PATH|- CONTAINER:DEST_PATH`,
		Short: "Copy files/folders between a container and the local filesystem",
		Long:  copyDescription,
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == "" {
				return fmt.Errorf("source can not be empty")
			}
			if args[1] == "" {
				return fmt.Errorf("destination can not be empty")
			}

			opts.source = args[0]
			opts.destination = args[1]
			return cc.runCopy(opts)
		},
		Example: copyExample(),
	}

	flags := cc.cmd.Flags()

	flags.BoolVarP(&opts.followLink, "follow-link", "L", false, "Always follow symbol link in SRC_PATH")

	cc.addFlags()
}

// addFlags adds flags for specific command.
func (cc *CopyCommand) addFlags() {
	flagSet := cc.cmd.Flags()
	flagSet.SetInterspersed(false)

	c := addCommonFlags(flagSet)
	cc.container = c
}

func splitCpArg(arg string) (container, path string) {
	if filepath.IsAbs(arg) {
		// Explicit local absolute path, e.g., `C:\foo` or `/foo`.
		return "", arg
	}

	parts := strings.SplitN(arg, ":", 2)

	if len(parts) == 1 || strings.HasPrefix(parts[0], ".") {
		// Either there's no `:` in the arg
		// OR it's an explicit local relative path like `./file:name.txt`.
		return "", arg
	}

	return parts[0], parts[1]
}


type copyDirection int

const (
	fromContainer copyDirection = (1 << iota)
	toContainer
	acrossContainers = fromContainer | toContainer
)

// runCreate is the entry of create command.
func (cc *CopyCommand) runCopy(opts copyOptions) error {
	srcContainer, srcPath := splitCpArg(opts.source)
	dstContainer, dstPath := splitCpArg(opts.destination)

	var direction copyDirection
	if srcContainer != "" {
		direction |= fromContainer
	}
	if dstContainer != "" {
		direction |= toContainer
	}

	cpParam := &cpConfig{
		followLink: opts.followLink,
	}

	ctx := context.Background()

	switch direction {
	case fromContainer:
		return copyFromContainer(ctx, cc.cli, srcContainer, srcPath, dstPath, cpParam)
	case toContainer:
		return fmt.Errorf("copy to containers is not supported")
	case acrossContainers:
		// Copying between containers isn't supported.
		return fmt.Errorf("copying between containers is not supported")
	default:
		// User didn't specify any container.
		return fmt.Errorf("must specify at least one container source")
	}
}

func resolveLocalPath(localPath string) (absPath string, err error) {
	if absPath, err = filepath.Abs(localPath); err != nil {
		return
	}

	return archive.PreserveTrailingDotOrSeparator(absPath, localPath), nil
}

func statContainerPath(ctx context.Context, cli *Cli, containerName, path string) (types.ContainerPathStat, error) {
	return cli.Client().ContainerStatPath(ctx, containerName, path)
}

func copyFromContainer(ctx context.Context, cli *Cli ,srcContainer, srcPath, dstPath string, cpParam *cpConfig) (err error) {
	if dstPath != "-" {
		// Get an absolute destination path.
		dstPath, err = resolveLocalPath(dstPath)
		if err != nil {
			return err
		}
	}

	// if client requests to follow symbol link, then must decide target file to be copied
	var rebaseName string
	if cpParam.followLink {
		srcStat, err := statContainerPath(ctx, cli, srcContainer, srcPath)

		// If the destination is a symbolic link, we should follow it.
		if err == nil && srcStat.Mode&os.ModeSymlink != 0 {
			linkTarget := srcStat.LinkTarget
			if !system.IsAbs(linkTarget) {
				// Join with the parent directory.
				srcParent, _ := archive.SplitPathDirEntry(srcPath)
				linkTarget = filepath.Join(srcParent, linkTarget)
			}

			linkTarget, rebaseName = archive.GetRebaseName(srcPath, linkTarget)
			srcPath = linkTarget
		}
	}

	content, stat, err := cli.Client().CopyFromContainer(ctx, srcContainer, srcPath)
	if err != nil {
		return err
	}
	defer content.Close()

	if dstPath == "-" {
		// Send the response to STDOUT.
		_, err = io.Copy(os.Stdout, content)

		return err
	}

	// Prepare source copy info.
	srcInfo := archive.CopyInfo{
		Path:       srcPath,
		Exists:     true,
		IsDir:      stat.Mode.IsDir(),
		RebaseName: rebaseName,
	}

	preArchive := content
	if len(srcInfo.RebaseName) != 0 {
		_, srcBase := archive.SplitPathDirEntry(srcInfo.Path)
		preArchive = archive.RebaseArchiveEntries(content, srcBase, srcInfo.RebaseName)
	}
	// See comments in the implementation of `archive.CopyTo` for exactly what
	// goes into deciding how and whether the source archive needs to be
	// altered for the correct copy behavior.
	return archive.CopyTo(preArchive, srcInfo, dstPath)
}


// copyExample shows examples in create command, and is used in auto-generated cli docs.
func copyExample() string {
	return `$ pouch cp 8assd1234:/root/foo /home
			$ pouch cp /home/bar 712yasbc:/root`
}


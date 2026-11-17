package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/canter0/canter/sdk"
	"github.com/canter0/canter/sdk/remote"
)

var errTransferSourceChanged = errors.New("source changed; create a new transfer plan")

func sandboxTransferCommand(direction string, args []string) error {
	flags := newSandboxFlags(direction)
	id := flags.fs.String("id", "", "sandbox ID")
	from := flags.fs.String("from", "", "source directory")
	to := flags.fs.String("to", "", "destination directory")
	resume := flags.fs.String("resume", "", "resume this transfer receipt")
	dry := flags.fs.Bool("dry-run", false, "show the manifest and collisions before transferring")
	overwrite := flags.fs.Bool("overwrite", false, "allow replacing files captured by the plan")
	sensitive := flags.fs.Bool("include-sensitive-files", false, "include sensitive filenames listed in the plan")
	if err := flags.parse(args); err != nil {
		return err
	}
	if *id == "" || *flags.timeout <= 0 {
		return errors.New("--id and a positive --timeout are required")
	}
	if direction == "transfer" || direction == "cancel-transfer" {
		if *resume == "" {
			return errors.New("--resume must name a transfer ID")
		}
	} else if *from == "" || *to == "" || *dry && *resume != "" {
		return errors.New("--from and --to are required, and --dry-run cannot be combined with --resume")
	}
	ctx, cancel := sandboxContext(*flags.timeout)
	defer cancel()
	client, err := flags.client(ctx)
	if err != nil {
		return err
	}
	switch direction {
	case "transfer", "cancel-transfer":
		var record sdk.SandboxTransfer
		if direction == "transfer" {
			record, err = client.Transfer(ctx, *id, *resume)
		} else {
			record, err = client.CancelTransfer(ctx, *id, *resume)
		}
		if err != nil {
			return err
		}
		return sandboxPrintJSON(record)
	}
	limits, err := client.TransferLimits(ctx, *id)
	if err != nil {
		return err
	}
	if limits.Files < 1 || limits.Bytes < 1 || limits.ChunkBytes < 1 || limits.ChunkBytes > 4<<20 || limits.MetadataBytes < 1 {
		return errors.New("server returned invalid transfer limits")
	}
	local, guest := *from, *to
	if direction == "pull" {
		local, guest = *to, *from
	}
	absolute, err := filepath.Abs(local)
	if err != nil {
		return err
	}
	if absolute == filepath.VolumeName(absolute)+string(os.PathSeparator) {
		return errors.New("choose a project directory below the filesystem root")
	}
	request := sdk.SandboxTransferRequest{Direction: direction, Path: guest, Overwrite: *overwrite, IncludeSensitive: *sensitive}
	binding := fmt.Sprintf("%x", sha256.Sum256([]byte(absolute)))
	var record sdk.SandboxTransfer
	if *resume != "" {
		record, err = client.Transfer(ctx, *id, *resume)
		if err != nil {
			return err
		}
		if record.Direction != direction || record.Path != guest || record.Overwrite != *overwrite || record.IncludeSensitive != *sensitive ||
			(record.DestinationBinding != "" && record.DestinationBinding != binding) {
			return errors.New("resume requires the original source, destination and flags")
		}
		if record.State == "complete" {
			return sandboxPrintJSON(record)
		}
		if record.State != "planned" {
			return fmt.Errorf("transfer %s is %s", record.ID, record.State)
		}
	}
	if direction == "push" {
		root, err := openTransferRoot(absolute, false)
		if err != nil {
			return err
		}
		defer root.Close()
		request.SandboxTransferManifest, err = localTransferManifest(ctx, root, limits, *sensitive)
		if err != nil {
			return err
		}
		if len(request.Entries) == 0 && guest != "/workspace" {
			request.Entries = []sdk.SandboxTransferEntry{{Path: ".", Directory: true}}
		}
		if *resume == "" {
			record, err = client.PlanTransfer(ctx, *id, request, *dry)
			if err != nil {
				return err
			}
		} else if !sameTransferSources(request.Entries, record.Entries) {
			return errTransferSourceChanged
		}
		if *dry {
			return sandboxPrintJSON(record)
		}
		showTransferPlan(record, absolute)
		if len(record.Collisions) > 0 && !record.Overwrite {
			return errors.New("destination collisions require a new plan with --overwrite")
		}
		err = pushSandboxTree(ctx, client, root, record, limits.ChunkBytes)
	} else {
		if *resume == "" {
			record, err = client.PlanTransfer(ctx, *id, request, *dry)
			if err != nil {
				return err
			}
		}
		// Inspect existing paths without creating them, including in dry-run.
		root, openErr := openTransferRoot(absolute, false)
		if openErr != nil && !errors.Is(openErr, os.ErrNotExist) {
			return openErr
		}
		if root != nil {
			defer root.Close()
		}
		if record.DestinationBinding == "" {
			versions := make([]string, len(record.Entries))
			for index, entry := range record.Entries {
				versions[index], err = localTransferVersion(root, entry.Path)
				if err != nil {
					return err
				}
			}
			if *dry {
				record.Destinations = versions
				for i, version := range versions {
					if version != "missing" && !(version == "directory" && record.Entries[i].Directory) {
						record.Collisions = append(record.Collisions, i)
					}
				}
				return sandboxPrintJSON(record)
			}
			if record.State == "complete" {
				if root == nil {
					empty, err := openTransferRoot(absolute, true)
					if err != nil {
						return err
					}
					empty.Close()
				}
				return sandboxPrintJSON(record)
			}
			record, err = client.BindTransferDestination(ctx, *id, record.ID, binding, versions)
			if err != nil {
				return err
			}
		}
		showTransferPlan(record, absolute)
		if len(record.Collisions) > 0 && !record.Overwrite {
			return errors.New("destination collisions require a new plan with --overwrite")
		}
		if root == nil {
			root, err = openTransferRoot(absolute, true)
			if err != nil {
				return err
			}
			defer root.Close()
		}
		err = pullSandboxTree(ctx, client, root, record)
	}
	resultCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer stop()
	latest, receiptErr := client.Transfer(resultCtx, *id, record.ID)
	if receiptErr == nil {
		_ = sandboxPrintJSON(latest)
	}
	if err != nil {
		return fmt.Errorf("transfer %s interrupted; completed files remain in place: %w", record.ID, err)
	}
	if receiptErr != nil {
		return fmt.Errorf("inspect transfer %s for its final receipt: %w", record.ID, receiptErr)
	}
	return nil
}

func showTransferPlan(record sdk.SandboxTransfer, local string) {
	fmt.Fprintf(os.Stderr, "Transfer %s · workspace %s · sandbox %s (generation %d)\n%s ↔ %s\n%d entries, %d bytes, %d excluded, %d collisions\n",
		record.ID, record.WorkspaceID, record.SandboxID, record.Generation, local, record.Path, len(record.Entries), record.TotalBytes, len(record.Exclusions), len(record.Collisions))
	for _, item := range record.Exclusions {
		fmt.Fprintf(os.Stderr, "Excluded %q (%s)\n", item.Path, item.Reason)
	}
	for _, index := range record.Collisions {
		if index >= 0 && index < len(record.Entries) {
			fmt.Fprintf(os.Stderr, "Collision %q\n", record.Entries[index].Path)
		}
	}
	if record.IncludeSensitive {
		for _, item := range record.Entries {
			for _, name := range strings.Split(item.Path, "/") {
				if sdk.SandboxTransferSensitiveName(name) {
					fmt.Fprintf(os.Stderr, "Including sensitive path %q\n", item.Path)
					break
				}
			}
		}
	}
	fmt.Fprintln(os.Stderr, "Filename exclusions cannot detect credentials embedded in source files. Use secret references for managed secrets.")
}

// Open one component at a time so spelled-out symlink parents are rejected.
func openTransferRoot(absolute string, create bool) (*os.Root, error) {
	volume := filepath.VolumeName(absolute) + string(os.PathSeparator)
	root, err := os.OpenRoot(volume)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(absolute, volume), string(os.PathSeparator)) {
		info, err := root.Lstat(part)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = root.Mkdir(part, 0755); err == nil || errors.Is(err, os.ErrExist) {
				info, err = root.Lstat(part)
			}
		}
		if err != nil {
			root.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, fmt.Errorf("transfer path %q is not a directory", part)
		}
		next, err := checkedTransferDirectory(root, part, info)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

func openTransferParent(root *os.Root, name string, create bool) (*os.Root, string, error) {
	if err := sdk.SandboxTransferRelativePath(name, true); err != nil {
		return nil, "", err
	}
	parts := strings.Split(name, "/")
	dir, err := root.OpenRoot(".")
	if err != nil {
		return nil, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		info, err := dir.Lstat(part)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = dir.Mkdir(part, 0755); err == nil || errors.Is(err, os.ErrExist) {
				info, err = dir.Lstat(part)
			}
		}
		if err != nil {
			dir.Close()
			return nil, "", err
		}
		if !info.IsDir() {
			dir.Close()
			return nil, "", fmt.Errorf("non-directory or symlink parent %q", part)
		}
		next, err := checkedTransferDirectory(dir, part, info)
		dir.Close()
		if err != nil {
			return nil, "", err
		}
		dir = next
	}
	return dir, parts[len(parts)-1], nil
}

func checkedTransferDirectory(parent *os.Root, name string, expected fs.FileInfo) (*os.Root, error) {
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	current, statErr := parent.Lstat(name)
	if err != nil || statErr != nil || !current.IsDir() || !os.SameFile(expected, opened) || !os.SameFile(expected, current) {
		child.Close()
		return nil, fmt.Errorf("transfer directory %q changed while opening", name)
	}
	return child, nil
}

func localTransferVersion(root *os.Root, name string) (string, error) {
	if root == nil {
		return "missing", nil
	}
	parent, leaf, err := openTransferParent(root, name, false)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "unsupported", nil
	}
	defer parent.Close()
	return transferLeafVersion(parent, leaf)
}

func transferLeafVersion(parent *os.Root, leaf string) (string, error) {
	info, err := parent.Lstat(leaf)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "directory", nil
	}
	if !singleTransferFile(info) {
		return "unsupported", nil
	}
	return transferLocalFingerprint(info), nil
}

func singleTransferFile(info fs.FileInfo) bool {
	return info.Mode().IsRegular() && transferHardLinks(info) == 1
}

func relativeTransferFile(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	parent, leaf, err := openTransferParent(root, name, false)
	if err != nil {
		return nil, nil, err
	}
	defer parent.Close()
	file, err := parent.OpenFile(leaf, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil || !singleTransferFile(info) {
		file.Close()
		return nil, nil, errors.New("only regular single-link files can be transferred")
	}
	return file, info, nil
}

func hashTransferFile(file *os.File, info fs.FileInfo) (string, error) {
	digest := sha256.New()
	if n, err := io.Copy(digest, io.LimitReader(file, info.Size()+1)); err != nil || n != info.Size() {
		return "", errTransferSourceChanged
	}
	after, err := file.Stat()
	if err != nil || transferLocalFingerprint(info) != transferLocalFingerprint(after) {
		return "", errTransferSourceChanged
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func localTransferManifest(ctx context.Context, root *os.Root, limits sdk.SandboxTransferLimits, includeSensitive bool) (sdk.SandboxTransferManifest, error) {
	var manifest sdk.SandboxTransferManifest
	excludedPaths := map[string]bool{}
	exclude := func(name, reason string) {
		if !excludedPaths[name] {
			excludedPaths[name] = true
			manifest.Exclusions = append(manifest.Exclusions, sdk.SandboxTransferExclusion{Path: name, Reason: reason})
		}
	}
	isGit := exec.CommandContext(ctx, "git", "-C", root.Name(), "rev-parse", "--show-toplevel").Run() == nil
	trackedOrUnignored := map[string]bool{}
	if isGit {
		cmd := exec.CommandContext(ctx, "git", "-C", root.Name(), "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", ".")
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return manifest, err
		}
		if err = cmd.Start(); err != nil {
			return manifest, err
		}
		maxBytes := int64(limits.MetadataBytes) * 2
		raw, err := io.ReadAll(io.LimitReader(pipe, maxBytes+1))
		if err != nil || int64(len(raw)) > maxBytes {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return manifest, errors.New("Git selection exceeds the transfer metadata limit")
		}
		if err = cmd.Wait(); err != nil {
			return manifest, err
		}
		for name := range bytes.SplitSeq(raw, []byte{0}) {
			if len(name) > 0 {
				trackedOrUnignored[string(name)] = true
			}
		}
	}
	var ignore []string
	if !isGit {
		file, _, err := relativeTransferFile(root, ".canterignore")
		if err == nil {
			scanner := bufio.NewScanner(io.LimitReader(file, 64<<10))
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line != "" && !strings.HasPrefix(line, "#") {
					ignore = append(ignore, strings.TrimSuffix(line, "/"))
				}
			}
			scanErr := scanner.Err()
			file.Close()
			if scanErr != nil {
				return manifest, scanErr
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return manifest, err
		}
	}
	var size int64
	err := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(manifest.Entries)+len(manifest.Exclusions) >= limits.Files {
			return errors.New("transfer entry limit exceeded")
		}
		reason := ""
		switch {
		case sdk.SandboxTransferPrivateName(d.Name()):
			reason = "platform or Git metadata"
		case !includeSensitive && sdk.SandboxTransferSensitiveName(d.Name()):
			reason = "sensitive filename"
		case isGit && !d.IsDir() && !trackedOrUnignored[name]:
			reason = "ignored by Git"
		case isGit && d.IsDir():
			// A tracked submodule is recorded as a single Git path.
			if trackedOrUnignored[name] {
				reason = "Git submodule"
			} else {
				check := exec.CommandContext(ctx, "git", "-C", root.Name(), "check-ignore", "-q", "--", name+"/")
				if check.Run() == nil {
					// Git ignore applies only to untracked paths. A forced
					// tracked file beneath an ignored directory still ships.
					prefix := name + "/"
					reason = "ignored by Git"
					for filepath := range trackedOrUnignored {
						if strings.HasPrefix(filepath, prefix) {
							reason = ""
							break
						}
					}
				}
			}
		case !isGit:
			switch d.Name() {
			case "node_modules", ".venv", "__pycache__", ".DS_Store", ".canterignore":
				reason = "default exclusion"
			}
			for _, pattern := range ignore {
				a, _ := path.Match(pattern, name)
				b, _ := path.Match(pattern, d.Name())
				if a || b {
					reason = "matched .canterignore"
					break
				}
			}
		}
		if reason == "" && sdk.SandboxTransferRelativePath(name, includeSensitive) != nil {
			reason = "unsupported path"
		}
		if reason == "" && !d.IsDir() && !d.Type().IsRegular() {
			reason = "unsupported entry"
		}
		if reason != "" {
			exclude(name, reason)
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			manifest.Entries = append(manifest.Entries, sdk.SandboxTransferEntry{Path: name, Directory: true})
			return nil
		}
		file, info, err := relativeTransferFile(root, name)
		if err != nil {
			if stat, infoErr := d.Info(); infoErr == nil && !singleTransferFile(stat) {
				exclude(name, "unsupported entry or hard link")
				return nil
			}
			return err
		}
		defer file.Close()
		if info.Size() > limits.Bytes-size {
			return errors.New("transfer byte limit exceeded")
		}
		digest, err := hashTransferFile(file, info)
		if err != nil {
			return err
		}
		size += info.Size()
		manifest.Entries = append(manifest.Entries, sdk.SandboxTransferEntry{Path: name, Size: info.Size(), SHA256: digest, Executable: info.Mode().Perm()&0111 != 0})
		return nil
	})
	return manifest, err
}

func sameTransferSources(a, b []sdk.SandboxTransferEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func pushSandboxTree(ctx context.Context, client *remote.SandboxClient, root *os.Root, plan sdk.SandboxTransfer, chunkSize int) error {
	for index, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt := plan.Receipts[index]
		if receipt.State == "complete" || receipt.State == "skipped" {
			continue
		}
		if receipt.State == "conflict" {
			return fmt.Errorf("%q: %s", entry.Path, receipt.Message)
		}
		if !entry.Directory {
			if err := pushTransferFile(ctx, client, root, plan, index, chunkSize); err != nil {
				return fmt.Errorf("%q: %w", entry.Path, err)
			}
		}
		if _, err := client.CompleteTransferFile(ctx, plan.SandboxID, plan.ID, index, entry.SHA256); err != nil {
			return fmt.Errorf("%q: %w", entry.Path, err)
		}
	}
	return nil
}

func pushTransferFile(ctx context.Context, client *remote.SandboxClient, root *os.Root, plan sdk.SandboxTransfer, index, chunkSize int) error {
	entry := plan.Entries[index]
	file, info, err := relativeTransferFile(root, entry.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	if info.Size() != entry.Size || (info.Mode().Perm()&0111 != 0) != entry.Executable {
		return errTransferSourceChanged
	}
	digest, err := hashTransferFile(file, info)
	if err != nil || digest != entry.SHA256 {
		return errTransferSourceChanged
	}
	receipt, err := client.BeginTransferFile(ctx, plan.SandboxID, plan.ID, index)
	if err != nil {
		return err
	}
	if receipt.State == "complete" {
		return nil
	}
	if receipt.Offset < 0 || receipt.Offset > entry.Size {
		return errors.New("invalid staged offset")
	}
	if _, err = file.Seek(receipt.Offset, io.SeekStart); err != nil {
		return err
	}
	buffer := make([]byte, chunkSize)
	for offset := receipt.Offset; offset < entry.Size; {
		part := buffer[:min(int64(len(buffer)), entry.Size-offset)]
		if _, err = io.ReadFull(file, part); err != nil {
			return errTransferSourceChanged
		}
		next, err := client.PutTransferChunk(ctx, plan.SandboxID, plan.ID, index, offset, part)
		if err != nil {
			return err
		}
		if next.Offset != offset+int64(len(part)) {
			return errors.New("invalid chunk receipt")
		}
		offset = next.Offset
	}
	after, err := file.Stat()
	if err != nil || transferLocalFingerprint(info) != transferLocalFingerprint(after) {
		return errTransferSourceChanged
	}
	return nil
}

func pullSandboxTree(ctx context.Context, client *remote.SandboxClient, root *os.Root, plan sdk.SandboxTransfer) error {
	for index, entry := range plan.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt := plan.Receipts[index]
		if receipt.State == "complete" || receipt.State == "skipped" {
			continue
		}
		if receipt.State == "conflict" {
			return fmt.Errorf("%q: %s", entry.Path, receipt.Message)
		}
		if err := pullTransferFile(ctx, client, root, plan, index); err != nil {
			return fmt.Errorf("%q: %w", entry.Path, err)
		}
		if _, err := client.CompleteTransferFile(ctx, plan.SandboxID, plan.ID, index, entry.SHA256); err != nil {
			return fmt.Errorf("%q: local file published; acknowledgement uncertain: %w", entry.Path, err)
		}
	}
	return nil
}

func pullTransferFile(ctx context.Context, client *remote.SandboxClient, root *os.Root, plan sdk.SandboxTransfer, index int) error {
	entry, expected := plan.Entries[index], plan.Destinations[index]
	parent, leaf, err := openTransferParent(root, entry.Path, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	current, err := transferLeafVersion(parent, leaf)
	if err != nil {
		return err
	}
	if entry.Directory {
		if current == "directory" {
			return nil
		}
		if current != "missing" {
			return errors.New("destination directory changed")
		}
		return parent.Mkdir(leaf, 0755)
	}
	// After an interrupted acknowledgement, confirm the exact bytes and
	// executable mode before treating the local publication as complete.
	if current != expected && current != "missing" && current != "unsupported" && current != "directory" {
		file, info, err := relativeTransferFile(root, entry.Path)
		if err == nil {
			actual, err := hashTransferFile(file, info)
			file.Close()
			if err == nil && actual == entry.SHA256 && (info.Mode().Perm()&0111 != 0) == entry.Executable {
				return nil
			}
		}
	}
	if current != expected || (current != "missing" && !plan.Overwrite) {
		return errors.New("destination changed; create a new transfer plan")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	name := ".canter-transfer-" + hex.EncodeToString(nonce[:])
	out, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer parent.Remove(name)
	defer out.Close()
	digest := sha256.New()
	for offset := int64(0); ; {
		chunk, err := client.ReadTransferChunk(ctx, plan.SandboxID, plan.ID, index, offset)
		if err != nil {
			return err
		}
		if chunk.Offset != offset || chunk.NextOffset != offset+int64(len(chunk.Data)) || chunk.NextOffset > entry.Size || chunk.Version != entry.Version ||
			chunk.Size != entry.Size || chunk.EOF != (chunk.NextOffset == entry.Size) || len(chunk.Data) == 0 && !chunk.EOF {
			return errors.New("invalid source chunk")
		}
		if _, err = io.MultiWriter(out, digest).Write(chunk.Data); err != nil {
			return err
		}
		offset = chunk.NextOffset
		if chunk.EOF {
			break
		}
	}
	if hex.EncodeToString(digest.Sum(nil)) != entry.SHA256 {
		return errTransferSourceChanged
	}
	if entry.Executable {
		err = out.Chmod(0755)
	} else {
		err = out.Chmod(0644)
	}
	if err != nil {
		return err
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	actual, err := transferLeafVersion(parent, leaf)
	if err != nil || actual != expected {
		return errors.New("destination changed while staging")
	}
	if expected == "missing" {
		return parent.Link(name, leaf)
	}
	return parent.Rename(name, leaf)
}

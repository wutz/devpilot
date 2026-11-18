/*
Copyright 2026 wutz.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command devpilot-commit freezes a DevBox root disk into an image.
//
// It runs in an unprivileged build pod with the root disk PVC (or a snapshot
// clone of it) mounted read-only. It reads the pinned base image manifest and
// config over HTTPS, scans upper/ into one increment layer, and appends that
// layer to the base manifest in the destination repository. Nothing from the
// base image's layers is downloaded.
//
// The result is written to the termination log so the operator can read it
// without another round trip to the registry.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/wutz/devpilot/internal/commit"
	"github.com/wutz/devpilot/internal/ext4"
	"github.com/wutz/devpilot/internal/initroot"
)

const (
	// defaultResultFile is where a Job's pod termination message is read from.
	defaultResultFile = "/dev/termination-log"
	// diskImageFile is the loop-backed ext4 root disk filename on shared storage.
	diskImageFile = "disk.img"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the command line, performs the commit and reports it. It is
// separate from main so the logic is testable and the exit code is explicit.
func run(args []string, stdout, stderr *os.File) int {
	logf := func(msg string, kv ...any) {
		fields := make([]string, 0, len(kv))
		for i := 0; i+1 < len(kv); i += 2 {
			fields = append(fields, fmt.Sprintf("%v=%v", kv[i], kv[i+1]))
		}
		line := msg
		if len(fields) > 0 {
			line += " " + strings.Join(fields, " ")
		}
		_, _ = fmt.Fprintf(stderr, "devpilot-commit: %s\n", line)
	}

	flags := flag.NewFlagSet("devpilot-commit", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var (
		rootDisk     string
		upperDir     string
		baseImage    string
		baseDigest   string
		repository   string
		tags         string
		compression  string
		excludes     string
		cleanCache   bool
		scratchDir   string
		username     string
		password     string
		passwordFile string
		plainHTTP    bool
		author       string
		comment      string
		envPairs     string
		workingDir   string
		cmdLine      string
		resultFile   string
		dryRun       bool
		chunkSize    int64
	)
	flags.StringVar(&rootDisk, "rootdisk", initroot.DefaultRootDisk,
		"Mount point of the root disk PVC; upper/ and .meta/ (or disk.img) are read from here.")
	flags.StringVar(&upperDir, "upper", "",
		"Scan this directory instead of <rootdisk>/upper; used for a snapshot clone.")
	flags.StringVar(&baseImage, "base-image", "",
		"Base image reference; defaults to the one recorded in .meta/base-image.json.")
	flags.StringVar(&baseDigest, "base-digest", "",
		"Base image digest; defaults to the one recorded in .meta/base-image.json.")
	flags.StringVar(&repository, "repository", "",
		"Destination repository without a tag, for example registry.example.com/library/devbox.")
	flags.StringVar(&tags, "tags", "",
		"Comma separated tags to push.")
	flags.StringVar(&compression, "compression", commit.CompressionZstd,
		"Layer compression: zstd, zstd:chunked, or gzip. A docker base image accepts gzip only.")
	flags.StringVar(&excludes, "exclude", "",
		"Comma separated extra globs to skip, relative to the layer root.")
	flags.BoolVar(&cleanCache, "clean-package-cache", true,
		"Strip apt archives and pip, uv and npm caches from the layer.")
	flags.StringVar(&scratchDir, "scratch-dir", os.Getenv("TMPDIR"),
		"Directory for the staged layer blob; defaults to the system temp dir.")
	flags.StringVar(&username, "username", os.Getenv("DEVPILOT_REGISTRY_USERNAME"),
		"Registry username; falls back to the image pull secret keychain.")
	flags.StringVar(&password, "password", os.Getenv("DEVPILOT_REGISTRY_PASSWORD"),
		"Registry password; prefer --password-file or the environment.")
	flags.StringVar(&passwordFile, "password-file", "",
		"Read the registry password from this file. A mounted secret avoids a password on the command line.")
	flags.BoolVar(&plainHTTP, "plain-http", false,
		"Talk to the registry over plain HTTP; for a local test registry.")
	flags.StringVar(&author, "author", "", "Set the image config author.")
	flags.StringVar(&comment, "comment", "", "Set the commit message recorded in the image history.")
	flags.StringVar(&envPairs, "env", "",
		"Comma separated NAME=VALUE entries to set in the image config.")
	flags.StringVar(&workingDir, "working-dir", "", "Replace the image config working directory.")
	flags.StringVar(&cmdLine, "cmd", "",
		"Replace the image config CMD; a shell quoted string, parsed like a Dockerfile CMD.")
	flags.StringVar(&resultFile, "result-file", defaultResultFile,
		"Write the JSON result here; the kubelet surfaces it as the pod termination message.")
	flags.BoolVar(&dryRun, "dry-run", false,
		"Stage the layer and report it without pushing anything.")
	flags.Int64Var(&chunkSize, "upload-chunk-size", commit.ChunkSize,
		"Bytes per registry upload request. The layer is sent in chunks this size and "+
			"resumed from the last acknowledged byte when one fails, so a proxy timeout "+
			"costs one chunk instead of the whole layer. 0 sends it in one request.")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	input, closeInput, err := openCommitInput(rootDisk, upperDir, logf)
	if err != nil {
		logf("Could not open the root disk", "err", err.Error())
		writeResult(stderr, resultFile, map[string]any{"error": err.Error()})
		return 1
	}
	defer closeInput()

	if err := resolveBaseMeta(input.meta, input.metaErr, &baseImage, &baseDigest); err != nil {
		logf("Could not determine the base image", "err", err.Error())
		return 1
	}
	if repository == "" {
		logf("Startup failed", "err", "a destination repository is required")
		return 2
	}
	destinationTags := splitList(tags)
	if len(destinationTags) == 0 {
		logf("Startup failed", "err", "at least one destination tag is required")
		return 2
	}

	if input.opaqueErr != nil {
		// The snapshot is an optimisation for the common case; an unreadable one
		// must not fail a commit that would otherwise be correct.
		logf("Could not read the opaque directory snapshot", "err", input.opaqueErr.Error())
	}
	if len(input.opaque) > 0 {
		logf("Read opaque directories", "count", len(input.opaque))
	}

	changes, err := buildChanges(author, comment, envPairs, workingDir, cmdLine)
	if err != nil {
		logf("Startup failed", "err", err.Error())
		return 2
	}

	secrets, err := readPassword(password, passwordFile)
	if err != nil {
		logf("Could not read the registry password", "err", err.Error())
		return 2
	}

	options, err := registryOptions(username, secrets)
	if err != nil {
		logf("Could not configure the registry client", "err", err.Error())
		return 2
	}
	authenticator, err := registryAuthenticator(username, secrets, repository)
	if err != nil {
		logf("Could not configure the registry client", "err", err.Error())
		return 2
	}
	var uploader commit.BlobUploader
	if chunkSize > 0 {
		uploader = &commit.RegistryUploader{
			ChunkSize:   chunkSize,
			Auth:        authenticator,
			NameOptions: nameOptions(plainHTTP),
			Logf:        func(msg string, kv ...any) { logf(msg, kv...) },
		}
	}

	request := commit.PushRequest{
		BaseReference:     baseImage,
		BaseDigest:        baseDigest,
		Repository:        repository,
		Tags:              destinationTags,
		UpperDir:          input.upperDir,
		Source:            input.source,
		OpaqueDirs:        input.opaque,
		ExcludePaths:      splitList(excludes),
		CleanPackageCache: cleanCache,
		Compression:       compression,
		Changes:           changes,
		ScratchDir:        scratchDir,
		Options:           options,
		NameOptions:       nameOptions(plainHTTP),
		Uploader:          uploader,
	}

	logf("Starting commit", "base", baseImage, "digest", baseDigest,
		"repository", repository, "tags", strings.Join(destinationTags, ","))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result, err := commit.Push(ctx, request)
	if err != nil {
		logf("Commit failed", "err", err.Error())
		writeResult(stderr, resultFile, map[string]any{"error": err.Error()})
		return 1
	}

	logf("Commit succeeded",
		"manifest", result.ManifestDigest.String(),
		"layer-size", result.LayerSize,
		"files", result.Stats.Files,
		"whiteouts", result.Stats.Whiteouts,
		"opaque-dirs", result.Stats.OpaqueDirs,
		"hardlinks", result.Stats.Hardlinks,
		"skipped", result.Stats.Skipped)

	summary := map[string]any{
		"baseDigest":     result.BaseDigest,
		"manifestDigest": result.ManifestDigest.String(),
		"configDigest":   result.ConfigDigest.String(),
		"diffID":         result.DiffID.String(),
		"layerSize":      result.LayerSize,
		"images":         result.Images,
		"stats":          result.Stats,
	}
	writeResult(stderr, resultFile, summary)
	printSummary(stdout, result)
	return 0
}

type commitInput struct {
	upperDir  string
	source    commit.Source
	meta      *initroot.BaseImageMeta
	metaErr   error
	opaque    []string
	opaqueErr error
}

// openCommitInput picks either the mounted directory tree (<rootdisk>/upper)
// or, when that is absent and <rootdisk>/disk.img exists, a read-only userspace
// ext4 parser over disk.img so unprivileged build pods need no loop device.
func openCommitInput(rootDisk, upperDir string, logf func(string, ...any)) (*commitInput, func(), error) {
	noop := func() {}
	if upperDir == "" {
		defaultUpper := filepath.Join(rootDisk, initroot.UpperDir)
		if _, err := os.Stat(defaultUpper); errors.Is(err, fs.ErrNotExist) {
			diskPath := filepath.Join(rootDisk, diskImageFile)
			if _, diskErr := os.Stat(diskPath); diskErr == nil {
				img, err := ext4.OpenFile(diskPath)
				if err != nil {
					return nil, noop, fmt.Errorf("open %s: %w", diskPath, err)
				}
				if logf != nil {
					logf("Opened ext4 root disk image",
						"path", diskPath,
						"needs-recovery", img.NeedsRecovery(),
						"replayed", img.Replayed())
					for _, w := range img.Warnings() {
						logf("ext4 image warning", "path", diskPath, "warning", w)
					}
				}
				meta, metaErr := readExt4BaseImageMeta(img)
				opaque, opaqueErr := readExt4OpaqueIndex(img)
				return &commitInput{
					source:    &commit.Ext4Source{Img: img, Root: initroot.UpperDir},
					meta:      meta,
					metaErr:   metaErr,
					opaque:    opaque,
					opaqueErr: opaqueErr,
				}, func() { _ = img.Close() }, nil
			}
		}
		upperDir = defaultUpper
	}

	meta, metaErr := initroot.ReadBaseImageMeta(rootDisk)
	opaque, opaqueErr := initroot.ReadOpaqueIndex(rootDisk)
	return &commitInput{
		upperDir:  upperDir,
		meta:      meta,
		metaErr:   metaErr,
		opaque:    opaque,
		opaqueErr: opaqueErr,
	}, noop, nil
}

func readExt4BaseImageMeta(img *ext4.Image) (*initroot.BaseImageMeta, error) {
	var meta initroot.BaseImageMeta
	rel := path.Join(initroot.MetaDir, initroot.BaseImageFile)
	if err := readExt4JSON(img, rel, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func readExt4OpaqueIndex(img *ext4.Image) ([]string, error) {
	var dirs []string
	rel := path.Join(initroot.MetaDir, initroot.OpaqueIndexFile)
	err := readExt4JSON(img, rel, &dirs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return dirs, nil
}

func readExt4JSON(img *ext4.Image, rel string, out any) error {
	ino, err := img.Lookup(rel)
	if err != nil {
		return err
	}
	sr, err := img.OpenSection(ino)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(sr)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", rel, err)
	}
	return nil
}

// resolveBase prefers the digest recorded in .meta/base-image.json. That file is
// what the devbox actually booted from, so it stays correct after a rebase
// changed the reference; a flag only fills the gap when the file is absent.
func resolveBase(rootDisk string, image, digest *string) error {
	meta, err := initroot.ReadBaseImageMeta(rootDisk)
	return resolveBaseMeta(meta, err, image, digest)
}

func resolveBaseMeta(meta *initroot.BaseImageMeta, err error, image, digest *string) error {
	switch {
	case err == nil:
		if meta.ImageRef != "" {
			*image = meta.ImageRef
		}
		if meta.ImageDigest != "" {
			*digest = meta.ImageDigest
		}
	case !errors.Is(err, fs.ErrNotExist):
		// A corrupt pin is worth reporting but not fatal: the explicit flags
		// still describe a usable base.
		if *image == "" {
			return fmt.Errorf("read base image pin: %w", err)
		}
	}
	if *image == "" {
		return fmt.Errorf("no base image reference in .meta/%s and none given by flag", initroot.BaseImageFile)
	}
	return nil
}

// buildChanges assembles the config overlay from the CLI.
func buildChanges(author, comment, envPairs, workingDir, cmdLine string) (commit.Changes, error) {
	changes := commit.Changes{Author: author, Comment: comment, WorkingDir: workingDir}
	for _, pair := range splitList(envPairs) {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return changes, fmt.Errorf("invalid env entry %q, want NAME=VALUE", pair)
		}
		changes.Env = append(changes.Env, commit.EnvVar{Name: key, Value: value})
	}
	if cmdLine != "" {
		argv, err := splitCommand(cmdLine)
		if err != nil {
			return changes, err
		}
		changes.Cmd = argv
	}
	return changes, nil
}

// splitCommand parses a CMD the way a Dockerfile does: a quoted string becomes
// shell words, an unquoted one stays a single argument so that
// "sleep infinity" is not confused with a program name containing a space.
func splitCommand(line string) ([]string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	if strings.HasPrefix(line, "[") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			return nil, fmt.Errorf("parse CMD as a JSON array: %w", err)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("CMD array is empty")
		}
		return argv, nil
	}
	if strings.ContainsAny(line, "\"'") {
		argv, err := splitQuoted(line)
		if err != nil {
			return nil, err
		}
		return argv, nil
	}
	argv := []string{line}
	return argv, nil
}

// splitQuoted splits on spaces while honouring single and double quotes.
func splitQuoted(line string) ([]string, error) {
	var (
		argv    []string
		current strings.Builder
		quote   rune
		opened  bool
	)
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			opened = true
		case r == ' ' || r == '\t':
			if current.Len() > 0 || opened {
				argv = append(argv, current.String())
				current.Reset()
				opened = false
			}
		default:
			current.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in CMD %q", line)
	}
	if current.Len() > 0 || opened {
		argv = append(argv, current.String())
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("CMD %q contains no arguments", line)
	}
	return argv, nil
}

// readPassword loads the registry password from the file when one is given, so
// a mounted secret never appears in the process list.
func readPassword(inline, file string) (string, error) {
	if file == "" {
		return inline, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

// nameOptions configures reference parsing. loopback hosts already default to
// http, so the flag only matters for a plain registry on a real hostname.
func nameOptions(plainHTTP bool) []name.Option {
	if plainHTTP {
		return []name.Option{name.Insecure}
	}
	return nil
}

// registryAuthenticator returns the credential the chunked uploader uses.
//
// It duplicates the choice registryOptions makes, because remote.Option values
// cannot be taken apart: the uploader needs the authenticator itself, not the
// opaque option that carries it.
func registryAuthenticator(username, password, repository string) (authn.Authenticator, error) {
	switch {
	case username != "" && password != "":
		return &authn.Basic{Username: username, Password: password}, nil
	case username != "":
		return nil, fmt.Errorf("a username was given without a password")
	default:
		repo, err := name.NewRepository(repository)
		if err != nil {
			return nil, fmt.Errorf("parse destination repository %q: %w", repository, err)
		}
		return authn.DefaultKeychain.Resolve(repo)
	}
}

// registryOptions builds the request options shared with the registry.
func registryOptions(username, password string) ([]remote.Option, error) {
	switch {
	case username != "" && password != "":
		return []remote.Option{remote.WithAuth(&authn.Basic{Username: username, Password: password})}, nil
	case username != "":
		return nil, fmt.Errorf("a username was given without a password")
	default:
		// A build Job mounts the destination pull secret; go-containerregistry
		// reads it from DOCKER_CONFIG through the default keychain.
		return []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}, nil
	}
}

// writeResult stores the JSON summary for the operator to read back.
func writeResult(stderr *os.File, resultPath string, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "devpilot-commit: Could not encode the result: %v\n", err)
		return
	}
	if err := os.WriteFile(resultPath, append(data, '\n'), 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "devpilot-commit: Could not write %s: %v\n", resultPath, err)
	}
}

// printSummary shows the pushed references on stdout, which is what a developer
// running the binary by hand wants to read.
func printSummary(stdout *os.File, result *commit.PushResult) {
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "MANIFEST\t%s\n", result.ManifestDigest.String())
	_, _ = fmt.Fprintf(w, "LAYER\t%s\t%d bytes\n", result.DiffID.String(), result.LayerSize)
	for _, image := range result.Images {
		_, _ = fmt.Fprintf(w, "IMAGE\t%s\n", image)
	}
	_ = w.Flush()
}

// splitList splits a comma separated flag, dropping blank entries.
func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

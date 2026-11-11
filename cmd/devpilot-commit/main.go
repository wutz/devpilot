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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/wutz/devpilot/internal/commit"
	"github.com/wutz/devpilot/internal/initroot"
)

// defaultResultFile is where a Job's pod termination message is read from.
const defaultResultFile = "/dev/termination-log"

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

	fs := flag.NewFlagSet("devpilot-commit", flag.ContinueOnError)
	fs.SetOutput(stderr)

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
	)
	fs.StringVar(&rootDisk, "rootdisk", initroot.DefaultRootDisk,
		"Mount point of the root disk PVC; upper/ and .meta/ are read from here.")
	fs.StringVar(&upperDir, "upper", "",
		"Scan this directory instead of <rootdisk>/upper; used for a snapshot clone.")
	fs.StringVar(&baseImage, "base-image", "",
		"Base image reference; defaults to the one recorded in .meta/base-image.json.")
	fs.StringVar(&baseDigest, "base-digest", "",
		"Base image digest; defaults to the one recorded in .meta/base-image.json.")
	fs.StringVar(&repository, "repository", "",
		"Destination repository without a tag, for example registry.example.com/library/devbox.")
	fs.StringVar(&tags, "tags", "",
		"Comma separated tags to push.")
	fs.StringVar(&compression, "compression", commit.CompressionZstd,
		"Layer compression: zstd or gzip. A docker base image accepts gzip only.")
	fs.StringVar(&excludes, "exclude", "",
		"Comma separated extra globs to skip, relative to the layer root.")
	fs.BoolVar(&cleanCache, "clean-package-cache", true,
		"Strip apt archives and pip, uv and npm caches from the layer.")
	fs.StringVar(&scratchDir, "scratch-dir", os.Getenv("TMPDIR"),
		"Directory for the staged layer blob; defaults to the system temp dir.")
	fs.StringVar(&username, "username", os.Getenv("DEVPILOT_REGISTRY_USERNAME"),
		"Registry username; falls back to the image pull secret keychain.")
	fs.StringVar(&password, "password", os.Getenv("DEVPILOT_REGISTRY_PASSWORD"),
		"Registry password; prefer --password-file or the environment.")
	fs.StringVar(&passwordFile, "password-file", "",
		"Read the registry password from this file. A mounted secret avoids a password on the command line.")
	fs.BoolVar(&plainHTTP, "plain-http", false,
		"Talk to the registry over plain HTTP; for a local test registry.")
	fs.StringVar(&author, "author", "", "Set the image config author.")
	fs.StringVar(&comment, "comment", "", "Set the commit message recorded in the image history.")
	fs.StringVar(&envPairs, "env", "",
		"Comma separated NAME=VALUE entries to set in the image config.")
	fs.StringVar(&workingDir, "working-dir", "", "Replace the image config working directory.")
	fs.StringVar(&cmdLine, "cmd", "",
		"Replace the image config CMD; a shell quoted string, parsed like a Dockerfile CMD.")
	fs.StringVar(&resultFile, "result-file", defaultResultFile,
		"Write the JSON result here; the kubelet surfaces it as the pod termination message.")
	fs.BoolVar(&dryRun, "dry-run", false,
		"Stage the layer and report it without pushing anything.")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if err := resolveBase(rootDisk, &baseImage, &baseDigest); err != nil {
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

	if upperDir == "" {
		upperDir = filepath.Join(rootDisk, initroot.UpperDir)
	}
	opaque, err := initroot.ReadOpaqueIndex(rootDisk)
	if err != nil {
		// The snapshot is an optimisation for the common case; an unreadable one
		// must not fail a commit that would otherwise be correct.
		logf("Could not read the opaque directory snapshot", "err", err.Error())
	}
	if len(opaque) > 0 {
		logf("Read opaque directories", "count", len(opaque))
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

	request := commit.PushRequest{
		BaseReference:     baseImage,
		BaseDigest:        baseDigest,
		Repository:        repository,
		Tags:              destinationTags,
		UpperDir:          upperDir,
		OpaqueDirs:        opaque,
		ExcludePaths:      splitList(excludes),
		CleanPackageCache: cleanCache,
		Compression:       compression,
		Changes:           changes,
		ScratchDir:        scratchDir,
		Options:           options,
		NameOptions:       nameOptions(plainHTTP),
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

// resolveBase prefers the digest recorded in .meta/base-image.json. That file is
// what the devbox actually booted from, so it stays correct after a rebase
// changed the reference; a flag only fills the gap when the file is absent.
func resolveBase(rootDisk string, image, digest *string) error {
	meta, err := initroot.ReadBaseImageMeta(rootDisk)
	switch {
	case err == nil:
		if meta.ImageRef != "" {
			*image = meta.ImageRef
		}
		if meta.ImageDigest != "" {
			*digest = meta.ImageDigest
		}
	case !os.IsNotExist(err):
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
func writeResult(stderr *os.File, path string, payload map[string]any) {
	data, err := json.Marshal(payload)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "devpilot-commit: Could not encode the result: %v\n", err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "devpilot-commit: Could not write %s: %v\n", path, err)
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

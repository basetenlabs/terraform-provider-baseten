package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/basetenlabs/baseten-go/client/modelarchive"
	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// modelPushPrivateState is the part of the framework's resource private state
// used here. The framework's concrete type lives in an internal package, so it
// can only be named structurally. Callers pass the concrete value from a
// request or response and check it for nil themselves, since a nil pointer in
// an interface does not compare equal to nil.
type modelPushPrivateState interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// modelPushHashesKey is where the per-file hash inventory lives in the
// resource's private state. Private state rather than an attribute because the
// inventory only exists to say which file caused a push: it is not rendered in
// plans, and it cannot be referenced from configuration, so it never becomes
// accidental API.
//
// It is strictly diagnostic and deliberately not the push trigger. Private
// state written during ModifyPlan is persisted only if an apply follows, so a
// no-op plan discards it; anything load-bearing would silently stop working the
// first time nothing else changed. The source_hash attribute is the baseline.
const modelPushHashesKey = "push_source_hashes"

// modelPushInlineConfigLabel stands in for an inline configuration in the hash
// inventory. It is deliberately not config.yaml, the file the inline config is
// written to, because the inventory is what change summaries are built from: a
// file name there would report moving between config_dir and config as an edit
// to that file, rather than as the swap of one whole source for another that it
// is. The angle brackets keep it from colliding with a real archive path.
const modelPushInlineConfigLabel = "<inline config>"

// modelPushChangesReported caps how many paths a change summary names, so a
// churning build directory cannot produce an unreadable wall of paths.
const modelPushChangesReported = 3

// modelPushMaxHashesStored caps how many entries are kept in private state.
// Private state is meant for small bookkeeping and rides along in every state
// version, so a directory past this size stores nothing and the change warning
// degrades to naming no files. Correctness does not depend on it.
const modelPushMaxHashesStored = 2000

// modelPushHashes is the per-file inventory, keyed by archive path.
type modelPushHashes struct {
	Files map[string]string `json:"files"`
}

// modelPushWalkHashes hashes every entry the archive would carry, using the
// same enumeration the upload runs on so the two cannot disagree about ignore
// rules, external package dirs, or a substituted config.yaml.
func modelPushWalkHashes(ctx context.Context, opts modelarchive.BuildModelArchiveOptions) (modelPushHashes, error) {
	files := map[string]string{}
	err := modelarchive.WalkModelArchive(ctx, opts, func(file modelarchive.File) error {
		reader, err := file.Open()
		if err != nil {
			return fmt.Errorf("open %s: %w", file.ArchivePath, err)
		}
		defer reader.Close()

		hasher := sha256.New()
		if _, err := io.Copy(hasher, reader); err != nil {
			return fmt.Errorf("hash %s: %w", file.ArchivePath, err)
		}
		files[file.ArchivePath] = hex.EncodeToString(hasher.Sum(nil))
		return nil
	})
	if err != nil {
		return modelPushHashes{}, err
	}
	return modelPushHashes{Files: files}, nil
}

// modelPushInlineHashes is the inventory for an inline config, which has no
// directory to walk. Canonical JSON is the hash input because encoding/json
// sorts map keys, so the same config always produces the same bytes regardless
// of the order it was written in HCL.
func modelPushInlineHashes(config map[string]any) (modelPushHashes, error) {
	canonical, err := json.Marshal(config)
	if err != nil {
		return modelPushHashes{}, fmt.Errorf("canonicalize config: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return modelPushHashes{
		Files: map[string]string{modelPushInlineConfigLabel: hex.EncodeToString(sum[:])},
	}, nil
}

// OverallHash folds the inventory into the single value stored in source_hash.
// Paths are sorted and length-delimited so no combination of path and hash can
// be rearranged into the same digest.
func (h modelPushHashes) OverallHash() string {
	paths := make([]string, 0, len(h.Files))
	for path := range h.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	hasher := sha256.New()
	for _, path := range paths {
		fmt.Fprintf(hasher, "%d:%s%d:%s", len(path), path, len(h.Files[path]), h.Files[path])
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// ChangeSummary describes how the inventory differs from a prior one, for the
// warning that explains why a push is happening. It returns the empty string
// when nothing changed.
func (h modelPushHashes) ChangeSummary(prior modelPushHashes) string {
	var added, removed, changed []string
	for path, hash := range h.Files {
		priorHash, existed := prior.Files[path]
		switch {
		case !existed:
			added = append(added, path)
		case priorHash != hash:
			changed = append(changed, path)
		}
	}
	for path := range prior.Files {
		if _, kept := h.Files[path]; !kept {
			removed = append(removed, path)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)

	var parts []string
	for label, paths := range map[string][]string{"added": added, "changed": changed, "removed": removed} {
		if len(paths) > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", label, modelPushJoinPaths(paths)))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// modelPushJoinPaths names at most modelPushChangesReported paths, summarizing
// the rest as a count so a churning build directory stays readable.
func modelPushJoinPaths(paths []string) string {
	if len(paths) <= modelPushChangesReported {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s, and %d more",
		strings.Join(paths[:modelPushChangesReported], ", "), len(paths)-modelPushChangesReported)
}

// modelPushReadHashes loads the inventory from private state. A missing or
// undecodable inventory is not an error: it only costs the file names in the
// change warning, so it reports absence rather than failing the plan.
func modelPushReadHashes(ctx context.Context, private modelPushPrivateState) (modelPushHashes, bool) {
	raw, diags := private.GetKey(ctx, modelPushHashesKey)
	if diags.HasError() || raw == nil {
		return modelPushHashes{}, false
	}
	var hashes modelPushHashes
	if err := json.Unmarshal(raw, &hashes); err != nil {
		return modelPushHashes{}, false
	}
	return hashes, true
}

// modelPushWriteHashes stores the inventory in private state, dropping it
// entirely when it is too large to belong there. Failures are swallowed for the
// same reason reads tolerate absence: the inventory is diagnostic, and losing it
// must not fail an apply.
func modelPushWriteHashes(ctx context.Context, private modelPushPrivateState, hashes modelPushHashes) {
	if len(hashes.Files) > modelPushMaxHashesStored {
		return
	}
	raw, err := json.Marshal(hashes)
	if err != nil {
		return
	}
	_ = private.SetKey(ctx, modelPushHashesKey, raw)
}

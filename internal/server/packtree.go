package server

import (
	"context"

	"github.com/nzinovev/agentum/internal/pack"
	"github.com/nzinovev/agentum/internal/worktree"
)

// commitTreeAdapter exposes the worktree manager's commit-tree reads as the
// pack package's CommitTree interface. It exists so the pack package stays a
// pure format package with no import of the git layer; the mapping of the
// worktree's TreeEntry onto pack's own shape happens here, once.
type commitTreeAdapter struct {
	manager *worktree.Manager
}

// FileAtCommit reads path exactly as it existed at commit.
func (adapter commitTreeAdapter) FileAtCommit(ctx context.Context, repoPath, commit, path string) ([]byte, error) {
	return adapter.manager.FileAtCommit(ctx, repoPath, commit, path)
}

// ListTreeAtCommit lists every entry under dir at commit, recursively.
func (adapter commitTreeAdapter) ListTreeAtCommit(ctx context.Context, repoPath, commit, dir string) ([]pack.TreeEntry, error) {
	entries, err := adapter.manager.ListTreeAtCommit(ctx, repoPath, commit, dir)
	if err != nil {
		return nil, err
	}
	mapped := make([]pack.TreeEntry, 0, len(entries))
	for _, entry := range entries {
		mapped = append(mapped, pack.TreeEntry{Path: entry.Path, Mode: entry.Mode, Size: entry.Size})
	}
	return mapped, nil
}

package api

import (
	"testing"

	"github.com/nzinovev/agentum/internal/store/sqlc"
)

// The registration git gate itself (not a work tree, no commits, shallow,
// linked worktree) is covered in internal/repoid against real repositories;
// what belongs here is the response shape.

func TestToProjectResponse_NilRelatedProjects(t *testing.T) {
	t.Parallel()
	// A project with no related set must serialize as [] not null — the public
	// shape stays stable regardless of the DB-side default.
	got := toProjectResponse(sqlc.Project{RepoIdentity: "git-roots:v1:abc", RelatedProjects: nil})
	if got.RelatedProjects == nil {
		t.Fatal("RelatedProjects must be a non-nil empty slice when the DB value is nil")
	}
	if len(got.RelatedProjects) != 0 {
		t.Fatalf("RelatedProjects = %v, want empty", got.RelatedProjects)
	}
}

func TestToProjectResponse_ExposesIdentityNotJustPath(t *testing.T) {
	t.Parallel()
	// repo_identity is part of the read-only shape: without it, a response
	// saying "this project already existed, the path was updated" cannot name
	// what stayed the same, and the move reads like an error rather than a
	// fact.
	got := toProjectResponse(sqlc.Project{RepoIdentity: "git-roots:v1:def", RepoPath: "/new"})
	if got.RepoIdentity != "git-roots:v1:def" {
		t.Fatalf("RepoIdentity = %q, want the stored identity", got.RepoIdentity)
	}

	// The registration-move fields are set by the handler, never by the row
	// mapping — a plain read must not claim runs are waiting anywhere.
	if got.PreviousRepoPath != "" || got.RunsAwaitingPreviousCheckout != 0 {
		t.Fatalf("plain response carries move fields: %+v", got)
	}
}

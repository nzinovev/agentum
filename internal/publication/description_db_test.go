package publication

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/artifacts"
	"github.com/nzinovev/agentum/internal/dbtest"
	"github.com/nzinovev/agentum/internal/publish"
)

func TestPublicationStoresScannedDescription(test *testing.T) {
	for _, policy := range []artifacts.ScanPolicy{artifacts.PolicyRedact, artifacts.PolicyReject} {
		test.Run(string(policy), func(test *testing.T) {
			for _, prose := range []string{"", "Add Bearer authentication to /settings", "Rotate the deploy password: correcthorsebatterystaple9", "secret: AGENTUM_WEBHOOK_SECRET_ENV should be read from env"} {
				test.Run(prose, func(test *testing.T) {
					database := dbtest.Store(test)
					const userID = "00000000-0000-0000-0000-000000000001"
					_, err := database.Store.DB.ExecContext(test.Context(), `
				WITH project AS (
					INSERT INTO projects (tenant_id, user_id, repo_identity, repo_root_commits, repo_path, name, related_projects)
					VALUES ($1, $2, 'description-fixture', '{}', '/tmp/description-fixture', 'fixture', '{}') RETURNING id
				)
				INSERT INTO runs (id, tenant_id, user_id, project_id, pipeline_pack, title, description, overrides, base_ref, state)
				SELECT $3, $1, $2, project.id, 'backend-development', 'fixture', 'fixture', '{}', 'main', 'awaiting_final_review' FROM project`,
						coordinatorTestTenant, userID, coordinatorTestRun)
					if err != nil {
						test.Fatal(err)
					}
					store := NewDescriptionStore(artifacts.SQLStoreDeps{DB: database.Store.DB, Queries: database.Queries, Blobs: artifacts.NewBlobStore(test.TempDir()), ScanPolicy: policy, Log: slog.New(slog.DiscardHandler)})
					var published []publish.DescriptionRef
					harness := newCoordinatorHarness(test, passingChecks(), scriptedPublisher{
						result:    publish.Result{BranchPushed: true, PullRequest: 42},
						onPublish: func(delivery publish.Delivery) { published = append(published, delivery.Description) },
					})
					harness.service.artifacts = store
					harness.store.run.UserID = userID
					secret := "ghp_" + strings.Repeat("x", 36)
					harness.store.run.Description = "Please remove \x00" + secret
					if prose != "" {
						harness.store.run.Description = prose
					}
					if err := harness.handle(test); err != nil {
						test.Fatal(err)
					}
					if policy == artifacts.PolicyReject && prose == "" {
						if len(published) != 0 || harness.store.row.State != "blocked" || harness.store.row.LastErrorCode.String != "secret_in_description" {
							test.Fatalf("provider calls=%d row=%+v", len(published), harness.store.row)
						}
						revisions, err := store.ListForRun(test.Context(), coordinatorTestTenant, coordinatorTestRun)
						if err != nil || len(revisions) != 0 {
							test.Fatalf("rejected revisions=%v err=%v", revisions, err)
						}
						return
					}
					if len(published) != 1 || harness.store.row.State != "published" {
						test.Fatalf("published=%v row=%+v", published, harness.store.row)
					}
					first := published[0]
					if prose == "" && (strings.Contains(first.Text, secret) || !strings.Contains(first.Text, "[REDACTED:github-pat]")) {
						test.Fatal("provider received unscanned text")
					}
					if prose != "" && !strings.Contains(first.Text, "\n\n"+prose+"\n\n") {
						test.Fatalf("prose changed: %q", first.Text)
					}
					stored, err := store.GetBytes(test.Context(), coordinatorTestTenant, first.RevisionID)
					if err != nil || !bytes.Equal(stored, []byte(first.Text)) {
						test.Fatalf("stored bytes differ from publication: %v", err)
					}
					harness.store.row.State = "pending"
					if err := harness.handle(test); err != nil {
						test.Fatal(err)
					}
					revisions, err := store.ListForRun(test.Context(), coordinatorTestTenant, coordinatorTestRun)
					if err != nil || len(revisions) != 1 || len(published) != 2 || published[1] != first {
						test.Fatalf("retry: revisions=%v published=%v err=%v", revisions, published, err)
					}
					if revisions[0].Kind != "pr_description" || revisions[0].Name != "publication/pr-description.md" || revisions[0].Actor != artifacts.ActorSystem {
						test.Fatalf("revision metadata=%+v", revisions[0])
					}
				})
			}
		})
	}
}

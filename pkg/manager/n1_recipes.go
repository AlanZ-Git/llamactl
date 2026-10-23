package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"net/url"
	"path"
	"strconv"
	"strings"
)

func validateN1Recipe(r *N1Recipe) error {
	if strings.TrimSpace(r.Description) == "" || len(r.Description) > 4096 || len(r.Tags) > 32 || len(r.Engine) > 256 || r.ArtifactDigest == "" || (r.Conclusion != "synthetic_only" && r.Conclusion != "unverified" && r.Conclusion != "business_validated") {
		return protocolError("invalid_config")
	}
	for _, name := range r.PortableMembers {
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, ":") || strings.ContainsRune(name, 92) {
			return protocolError("invalid_config")
		}
	}
	for _, tag := range r.Tags {
		if len(tag) > 128 || tag == "" {
			return protocolError("invalid_config")
		}
	}
	patch, err := instance.NormalizeControlPatch(n1JSON(r.Deployment))
	if err != nil {
		return protocolError("unsupported_parameter")
	}
	r.Deployment = patch
	return instance.ValidateRequestContract(r.Request)
}
func candidateDigest(c N1Candidate) string {
	return database.N1Digest(n1JSON(map[string]any{"deployment": c.Deployment, "request": c.Request}))
}
func recipeCandidate(r N1Recipe) N1Candidate {
	return N1Candidate{ArtifactID: r.ArtifactID, ArtifactDigest: r.ArtifactDigest, Deployment: r.Deployment, Request: r.Request}
}
func (im *instanceManager) n1RecipeApplicable(ctx context.Context, n *n1Coordinator, r N1Recipe) error {
	if r.Foreign || r.Engine != im.globalConfig.N1.EngineIdentity {
		return protocolError("recipe_unverified")
	}
	a, err := n.store.Artifact(ctx, r.ArtifactID)
	if err != nil {
		return err
	}
	if a.ManifestDigest != r.ArtifactDigest {
		return protocolError("artifact_changed")
	}
	_, _, err = n.inventory.Resolve(ctx, a)
	return err
}
func (im *instanceManager) n1RecipeEvidence(ctx context.Context, n *n1Coordinator, r N1Recipe) error {
	if r.Evidence == nil || r.Foreign {
		return protocolError("recipe_unverified")
	}
	run, err := n.store.Get(ctx, r.Evidence.RunID)
	if err != nil {
		return err
	}
	if run.Instance != im.globalConfig.N1.Instance || run.FrozenAt == nil {
		return protocolError("recipe_unverified")
	}
	events, err := n.store.Events(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Seq != r.Evidence.FindingsSeq {
			continue
		}
		if e.Type != "findings_v1" || e.Digest != r.Evidence.Digest {
			return protocolError("recipe_unverified")
		}
		var f N1Findings
		if json.Unmarshal(e.Payload, &f) != nil {
			return protocolError("recipe_unverified")
		}
		for _, c := range f.Candidates {
			if c.Candidate == r.Evidence.Candidate && c.ArtifactDigest == r.ArtifactDigest && c.ParameterDigest == candidateDigest(recipeCandidate(r)) && c.Quality == r.Conclusion && len(c.Attempts) > 0 && c.Decision == "shortlisted" && f.Conclusion == r.Conclusion {
				return nil
			}
		}
	}
	return protocolError("recipe_unverified")
}
func (im *instanceManager) n1Recipes(ctx context.Context, n *n1Coordinator, inst *instance.Instance, scope string, admin bool, method string, parts []string, query url.Values, body []byte) (int, any, error) {
	if len(parts) == 1 && method == "GET" {
		rows, err := n.store.Recipes(ctx, inst.Name)
		if err != nil {
			return 0, nil, err
		}
		out := []any{}
		for _, row := range rows {
			var doc N1Recipe
			_ = json.Unmarshal(row.Document, &doc)
			if query.Get("published") == "true" && row.Publication != "published" {
				continue
			}
			if tag := query.Get("task"); tag != "" {
				found := false
				for _, v := range doc.Tags {
					found = found || v == tag
				}
				if !found {
					continue
				}
			}
			if query.Get("verified") == "true" && im.n1RecipeEvidence(ctx, n, doc) != nil {
				continue
			}
			out = append(out, map[string]any{"recipe_id": row.ID, "version": row.Version, "recipe": doc, "recipe_digest": row.Digest, "publication_state": row.Publication, "effective_available": row.Publication == "published" && im.n1RecipeApplicable(ctx, n, doc) == nil})
		}
		return 200, map[string]any{"recipes": out}, nil
	}
	if len(parts) == 1 && method == "POST" {
		var doc N1Recipe
		if err := n1Decode(body, &doc); err != nil {
			return 0, nil, err
		}
		if doc.Foreign || doc.SourceRef != "" {
			return 0, nil, protocolError("invalid_config")
		}
		if err := validateN1Recipe(&doc); err != nil {
			return 0, nil, err
		}
		// Portable names are descriptive evidence from the local verified artifact,
		// never a remote path mapping or a source of execution authority.
		if a, err := n.store.Artifact(ctx, doc.ArtifactID); err == nil {
			doc.PortableMembers = nil
			for _, member := range a.Members {
				doc.PortableMembers = append(doc.PortableMembers, member.Name)
			}
		}
		id := doc.ID
		if id == "" {
			if doc.ExpectedVersion != 0 {
				return 0, nil, protocolError("revision_conflict")
			}
			id = newControlID()
		} else {
			if doc.ExpectedVersion < 1 {
				return 0, nil, protocolError("revision_conflict")
			}
			if _, err := n.store.GetRecipe(ctx, inst.Name, id, doc.ExpectedVersion); err != nil {
				return 0, nil, err
			}
		}
		version := doc.ExpectedVersion + 1
		doc.ID = ""
		doc.ExpectedVersion = 0
		row, _, err := n.store.PutRecipe(ctx, database.RecipeRecord{Instance: inst.Name, ID: id, Version: version, Document: n1JSON(doc), ArtifactID: doc.ArtifactID, ArtifactDigest: doc.ArtifactDigest, CreatedBy: scope})
		return 201, row, err
	}
	if len(parts) == 2 && parts[1] == "import" && method == "POST" {
		status, out, err := im.n1Import(ctx, n, inst, scope, body)
		if status == 409 && !admin {
			keyID, _ := strconv.Atoi(strings.TrimPrefix(scope, "key:"))
			readable, checkErr := im.N1Capability(ctx, keyID, inst.ID, "discover")
			if checkErr != nil || !readable {
				out = map[string]any{"error": "recipe_conflict"}
			}
		}
		return status, out, err
	}
	if len(parts) < 4 || parts[2] != "versions" {
		return 0, nil, protocolError("n1_not_found")
	}
	version, err := strconv.Atoi(parts[3])
	if err != nil {
		return 0, nil, protocolError("invalid_config")
	}
	row, err := n.store.GetRecipe(ctx, inst.Name, parts[1], version)
	if err != nil {
		return 0, nil, err
	}
	var doc N1Recipe
	if err = json.Unmarshal(row.Document, &doc); err != nil {
		return 0, nil, err
	}
	if len(parts) == 4 && method == "GET" {
		return 200, map[string]any{"recipe_id": row.ID, "version": row.Version, "recipe": doc, "recipe_digest": row.Digest, "publication_state": row.Publication, "effective_available": row.Publication == "published" && im.n1RecipeApplicable(ctx, n, doc) == nil}, nil
	}
	if len(parts) == 5 {
		if parts[4] == "export" && method == "GET" {
			return 200, map[string]any{"schema_version": 1, "recipe_id": row.ID, "version": row.Version, "recipe_digest": row.Digest, "recipe": doc}, nil
		}
		if method == "POST" && (parts[4] == "publish" || parts[4] == "withdraw") {
			var req struct {
				Version    int    `json:"expected_version"`
				Conclusion string `json:"conclusion_level"`
			}
			if err = n1Decode(body, &req); err != nil {
				return 0, nil, err
			}
			if req.Version != version {
				return 0, nil, protocolError("revision_conflict")
			}
			state := "withdrawn"
			if parts[4] == "publish" {
				state = "published"
				if req.Conclusion != doc.Conclusion {
					return 0, nil, protocolError("recipe_unverified")
				}
				if err = im.n1RecipeApplicable(ctx, n, doc); err != nil {
					return 0, nil, err
				}
				if err = im.n1RecipeEvidence(ctx, n, doc); err != nil {
					return 0, nil, err
				}
			}
			if err = n.store.PublishRecipe(ctx, inst.Name, row.ID, version, scope, state); err != nil {
				return 0, nil, err
			}
			return 200, map[string]any{"recipe_id": row.ID, "version": version, "publication_state": state}, nil
		}
	}
	return 0, nil, protocolError("n1_not_found")
}
func (im *instanceManager) n1Import(ctx context.Context, n *n1Coordinator, inst *instance.Instance, scope string, body []byte) (int, any, error) {
	var req struct {
		DryRun   *bool `json:"dry_run"`
		AsNew    bool  `json:"as_new"`
		Snapshot struct {
			Schema  int      `json:"schema_version"`
			ID      string   `json:"recipe_id"`
			Version int      `json:"version"`
			Digest  string   `json:"recipe_digest"`
			Recipe  N1Recipe `json:"recipe"`
		} `json:"snapshot"`
	}
	if err := n1Decode(body, &req); err != nil {
		return 0, nil, err
	}
	s := req.Snapshot
	if req.DryRun == nil || s.Schema != 1 || s.ID == "" || s.Version < 1 || database.N1Digest(n1JSON(s.Recipe)) != s.Digest {
		return 0, nil, protocolError("invalid_config")
	}
	if err := validateN1Recipe(&s.Recipe); err != nil {
		return 0, nil, err
	}
	if !req.AsNew {
		if old, err := n.store.GetRecipe(ctx, inst.Name, s.ID, s.Version); err == nil {
			var imported N1Recipe
			_ = json.Unmarshal(old.Document, &imported)
			// Import deliberately strips local evidence and changes the stored digest.
			// The immutable source reference retains the original snapshot identity.
			sameSource := imported.Foreign && imported.SourceRef == fmt.Sprintf("%s/%d/%s", s.ID, s.Version, s.Digest)
			if old.Digest == s.Digest || sameSource {
				return 200, map[string]any{"idempotent": true, "recipe": old}, nil
			}
			return 409, map[string]any{"error": "recipe_conflict", "existing_recipe_digest": old.Digest, "imported_recipe_digest": s.Digest, "existing_artifact_manifest_digest": old.ArtifactDigest, "imported_artifact_manifest_digest": s.Recipe.ArtifactDigest}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, nil, err
		}
	}
	doc := s.Recipe
	doc.Foreign = true
	doc.Evidence = nil
	doc.Conclusion = "unverified"
	doc.SourceRef = fmt.Sprintf("%s/%d/%s", s.ID, s.Version, s.Digest)
	doc.ArtifactID = ""
	if a, err := n.store.ArtifactByDigest(ctx, doc.ArtifactDigest); err == nil {
		if n.inventory.Check(ctx, a) == nil {
			doc.ArtifactID = a.ID
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, err
	}
	if *req.DryRun {
		return 200, map[string]any{"dry_run": true, "recipe": doc, "resolved": doc.ArtifactID != "", "publication_state": "draft"}, nil
	}
	id, version := s.ID, s.Version
	if req.AsNew {
		id = newControlID()
		version = 1
	}
	row, _, err := n.store.PutRecipe(ctx, database.RecipeRecord{Instance: inst.Name, ID: id, Version: version, Document: n1JSON(doc), ArtifactID: doc.ArtifactID, ArtifactDigest: doc.ArtifactDigest, CreatedBy: scope})
	return 201, row, err
}

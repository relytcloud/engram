package memorylake

import (
	"fmt"
	"net/url"
	"strings"
)

// wsItem is one entry of GET /api/v3/workspaces.
type wsItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CustomID string `json:"custom_id"`
}

// listAllWorkspaces returns every workspace MemoryLake has recorded,
// following continuation_token across pages (see listAllPages) rather than
// reading only the first page — a caller with more workspaces than fit on
// one page must still be resolvable by ResolveWorkspaceID.
func (c *Client) listAllWorkspaces() ([]wsItem, error) {
	return listAllPages[wsItem](c, maxPaginationPages, "listAllWorkspaces", func(token string) string {
		path := "/api/v3/workspaces?page_size=200"
		if token != "" {
			path += "&continuation_token=" + url.QueryEscape(token)
		}
		return path
	})
}

// ResolveWorkspaceID turns a human-friendly workspace reference (its
// custom_id or display name) into the MemoryLake workspace ID ("ws-...").
// If x already looks like a workspace ID (the "ws-" prefix), it is returned
// as-is with no HTTP call.
func (c *Client) ResolveWorkspaceID(x string) (string, error) {
	if strings.HasPrefix(x, "ws-") {
		return x, nil
	}
	items, err := c.listAllWorkspaces()
	if err != nil {
		return "", err
	}
	for _, w := range items {
		if w.CustomID == x || w.Name == x {
			return w.ID, nil
		}
	}
	return "", &APIError{Code: "NOT_FOUND", Message: "workspace not found: " + x}
}

// projItem is one entry of GET /api/v3/workspaces/{ws}/projects.
type projItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CustomID string `json:"custom_id"`
}

// listAllProjects returns every project in workspace ws, following
// continuation_token across pages (see listAllPages). Shared by
// EnsureProject and, in backend.go, ProjectExists/ListProjectNames — all
// three need the complete project list, not just the first page, to give a
// correct answer for workspaces with more projects than fit on one page.
func (c *Client) listAllProjects(ws string) ([]projItem, error) {
	what := fmt.Sprintf("listAllProjects for workspace %s", ws)
	return listAllPages[projItem](c, maxPaginationPages, what, func(token string) string {
		path := "/api/v3/workspaces/" + ws + "/projects?page_size=200"
		if token != "" {
			path += "&continuation_token=" + url.QueryEscape(token)
		}
		return path
	})
}

// EnsureProject finds the MemoryLake project matching name within workspace
// ws (matched by custom_id or name), creating it via POST if it does not yet
// exist. Callers may invoke this repeatedly with the same (ws, name): once
// created, subsequent calls find the existing project and do not re-POST.
func (c *Client) EnsureProject(ws, name string) (string, error) {
	items, err := c.listAllProjects(ws)
	if err != nil {
		return "", err
	}
	for _, p := range items {
		if p.CustomID == name || p.Name == name {
			return p.ID, nil
		}
	}
	var created struct {
		ID string `json:"id"`
	}
	body := map[string]any{"custom_id": name, "name": name}
	if err := c.doJSON("POST", "/api/v3/workspaces/"+ws+"/projects", body, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// actorItem is one entry of GET /api/v3/actors.
type actorItem struct {
	ID       string `json:"id"`
	CustomID string `json:"custom_id"`
}

// listAllActors returns every actor MemoryLake has recorded, following
// continuation_token across pages (see listAllPages). Used by EnsureActor's
// CUSTOM_ID_CONFLICT recovery path (see below) to find the actor id
// belonging to a custom_id that already exists.
func (c *Client) listAllActors() ([]actorItem, error) {
	return listAllPages[actorItem](c, maxPaginationPages, "listAllActors", func(token string) string {
		path := "/api/v3/actors?page_size=200"
		if token != "" {
			path += "&continuation_token=" + url.QueryEscape(token)
		}
		return path
	})
}

// EnsureActor creates a HUMAN actor identified by customID (e.g. a
// per-machine identifier) and binds it to workspace ws, then returns the
// actor ID.
//
// POST /api/v3/actors is not idempotent on custom_id the way
// ensureConversation/EnsureProject's creation calls are: creating an actor
// whose custom_id already exists fails with error_code CUSTOM_ID_CONFLICT
// instead of returning the existing actor. Retrying that path here (by
// paging through GET /api/v3/actors to find the actor already carrying
// customID) is what keeps repeated EnsureActor calls — e.g. across engram
// processes on the same machine, or a machine whose actor was created by a
// previous run — from either failing outright or, worse, racing to create a
// second, orphaned actor for the same identity (spec §11.5).
func (c *Client) EnsureActor(ws, customID, displayName string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	body := map[string]any{
		"custom_id":    customID,
		"actor_type":   "HUMAN",
		"display_name": displayName,
	}

	actorID := ""
	postErr := c.doJSON("POST", "/api/v3/actors", body, &created)
	if postErr == nil {
		actorID = created.ID
	} else {
		apiErr, ok := postErr.(*APIError)
		if !ok || apiErr.Code != "CUSTOM_ID_CONFLICT" {
			return "", postErr
		}
		actors, listErr := c.listAllActors()
		if listErr != nil {
			return "", listErr
		}
		for _, a := range actors {
			if a.CustomID == customID {
				actorID = a.ID
				break
			}
		}
		if actorID == "" {
			// The server reported a conflict but the actor list doesn't
			// contain a matching custom_id (e.g. a race with a delete, or a
			// list endpoint that doesn't yet reflect the conflicting write).
			// Surface the original conflict rather than masking it behind a
			// confusing "actor not found".
			return "", postErr
		}
	}

	// Bind the actor to the workspace. Binding is documented as idempotent
	// on the MemoryLake side, so no existence check is needed here — this
	// runs the same way whether the actor was just created or recovered via
	// the CUSTOM_ID_CONFLICT path above.
	if err := c.doJSON("POST", "/api/v3/workspaces/"+ws+"/actors", map[string]any{"actor_id": actorID}, nil); err != nil {
		return "", err
	}
	return actorID, nil
}

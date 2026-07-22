package memorylake

import "strings"

// wsItem is one entry of GET /api/v3/workspaces.
type wsItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CustomID string `json:"custom_id"`
}

// ResolveWorkspaceID turns a human-friendly workspace reference (its
// custom_id or display name) into the MemoryLake workspace ID ("ws-...").
// If x already looks like a workspace ID (the "ws-" prefix), it is returned
// as-is with no HTTP call.
func (c *Client) ResolveWorkspaceID(x string) (string, error) {
	if strings.HasPrefix(x, "ws-") {
		return x, nil
	}
	// TODO(pagination): GET /api/v3/workspaces may be cursor-paginated
	// (continuation_token) once a caller has enough workspaces. First cut
	// only reads the first page. See spec §11.5.
	var out struct {
		Items []wsItem `json:"items"`
	}
	if err := c.doJSON("GET", "/api/v3/workspaces", nil, &out); err != nil {
		return "", err
	}
	for _, w := range out.Items {
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

// EnsureProject finds the MemoryLake project matching name within workspace
// ws (matched by custom_id or name), creating it via POST if it does not yet
// exist. Callers may invoke this repeatedly with the same (ws, name): once
// created, subsequent calls find the existing project and do not re-POST.
func (c *Client) EnsureProject(ws, name string) (string, error) {
	// TODO(pagination): GET .../projects may be cursor-paginated
	// (continuation_token) for workspaces with many projects. First cut
	// only reads the first page. See spec §11.5.
	var out struct {
		Items []projItem `json:"items"`
	}
	if err := c.doJSON("GET", "/api/v3/workspaces/"+ws+"/projects", nil, &out); err != nil {
		return "", err
	}
	for _, p := range out.Items {
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

// EnsureActor creates a HUMAN actor identified by customID (e.g. a
// per-machine identifier) and binds it to workspace ws, then returns the
// actor ID.
//
// TODO(spike, spec §11.5): the real "actor already exists" error code from
// POST /api/v3/actors has not been observed yet — this repo has no live
// MemoryLake instance to test against. First cut assumes customID is unique
// per machine and always POSTs to create. Once the real duplicate error code
// is known, branch on it here and fall back to listing actors (there is no
// documented GET /api/v3/actors list endpoint yet either) to recover the
// existing actor ID instead of failing.
func (c *Client) EnsureActor(ws, customID, displayName string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	body := map[string]any{
		"custom_id":    customID,
		"actor_type":   "HUMAN",
		"display_name": displayName,
	}
	if err := c.doJSON("POST", "/api/v3/actors", body, &created); err != nil {
		return "", err
	}
	// Bind the actor to the workspace. Binding is documented as idempotent
	// on the MemoryLake side, so no existence check is needed here.
	if err := c.doJSON("POST", "/api/v3/workspaces/"+ws+"/actors", map[string]any{"actor_id": created.ID}, nil); err != nil {
		return "", err
	}
	return created.ID, nil
}

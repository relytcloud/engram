package memorylake

// ListAllFacts exposes the read-only fact listing for eval dataset
// construction (eval/cmd/evalrun -suite dump-facts). It performs no writes.
func (c *Client) ListAllFacts(ws, projID string) ([]Fact, error) {
	return c.listAllFacts(ws, projID)
}

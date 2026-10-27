package opencode

// Existing fixtures describe V1 endpoints only. Automatic negotiation has its
// own tests against both protocols rather than teaching every V1 fixture V2.
func newV1TestClient(baseURL, password string) *Client {
	c := New(baseURL, password)
	c.protocol = 1
	return c
}

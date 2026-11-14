package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// MaxRenewBytes caps a renewal request and its reply. Both carry one
// certificate or one certificate request.
const MaxRenewBytes = 16 << 10

// CertInvalidMessage is the error text the node API sends, with 401, when the
// client certificate is not valid at the server's clock: it has expired, or
// it is not valid yet. ErrCertInvalid is what the client returns for it.
const CertInvalidMessage = "the client certificate is expired or not yet valid"

// ErrCertInvalid means the server refused the node's certificate as expired
// or not yet valid.
var ErrCertInvalid = errors.New(CertInvalidMessage)

// RenewPath is the route a node renews its certificate on. The node
// authenticates with its current certificate, which must still be valid.
func RenewPath(id string) string { return Prefix + "/nodes/" + id + "/renew" }

// RenewRequest is the body of POST renew. CSR is a DER certificate request
// for the node's new key. The server takes only the key from it; the node ID
// comes from the client certificate.
type RenewRequest struct {
	CSR []byte `json:"csr"`
}

// RenewReply is the reply to POST renew: the node's new certificate, PEM.
type RenewReply struct {
	CertPEM string `json:"cert_pem"`
}

// Renew asks the server for a new certificate for the public key in csr. The
// caller checks the reply against the CA, the node ID and its key before it
// uses it.
func (c *Client) Renew(ctx context.Context, nodeID string, csr []byte) ([]byte, error) {
	b, err := json.Marshal(RenewRequest{CSR: csr})
	if err != nil {
		return nil, err
	}
	var reply RenewReply
	if err := c.do(ctx, http.MethodPost, RenewPath(nodeID), b, &reply); err != nil {
		return nil, err
	}
	if reply.CertPEM == "" {
		return nil, errors.New("the server sent no certificate")
	}
	return []byte(reply.CertPEM), nil
}

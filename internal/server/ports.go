package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The port registry is a ledger of which ports services use on each machine of the
// network. Agents that deploy things (a Docker stack, a dev server) otherwise pick ports
// blind and collide with each other; shartifacts is the one place they all connect to, so
// it keeps the list. It lives in the workspace like everything else: the network guard
// and sign-in decide who reaches it, and the share hostnames never serve it.

type portClaimInput struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	From     int    `json:"from,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Service  string `json:"service"`
	Owner    string `json:"owner,omitempty"`
	Note     string `json:"note,omitempty"`
}

// claimPort records a claim, naming the agent token that made it as its owner when the
// claim names none.
func (s *Server) claimPort(in portClaimInput, agent string) (store.PortClaim, bool, error) {
	if in.Owner == "" {
		in.Owner = agent
	}
	return s.Store.ClaimPort(store.PortClaim{Host: in.Host, Port: in.Port, Protocol: in.Protocol, Service: in.Service, Owner: in.Owner, Note: in.Note}, in.From)
}

// agentName is the name of the agent token that signed r in: empty for a browser, and
// while sign-in is off, when nothing says who is asking.
func agentName(r *http.Request) string {
	if c, ok := credentialFrom(r); ok {
		return c.token.Name
	}
	return ""
}

func (s *Server) portRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ports", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.Store.Ports(r.URL.Query().Get("host"))
		respond(w, 200, map[string]any{"claims": list, "hosts": s.Store.PortHosts()}, err)
	})
	mux.HandleFunc("POST /api/ports", func(w http.ResponseWriter, r *http.Request) {
		var in portClaimInput
		if !decode(w, r, &in) {
			return
		}
		c, created, err := s.claimPort(in, agentName(r))
		var taken *store.PortTakenError
		if errors.As(err, &taken) {
			// Scripts get the holder and the way out as fields, not only in the message.
			writeJSON(w, 409, map[string]any{"error": err.Error(), "code": "port_taken", "holder": taken.Holder, "next_free": taken.Next})
			return
		}
		status := 200
		if created {
			status = 201
		}
		respond(w, status, c, err)
	})
	mux.HandleFunc("DELETE /api/ports/{host}/{port}", func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.Atoi(r.PathValue("port"))
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "port must be a number"})
			return
		}
		_, released, err := s.Store.ReleasePort(r.PathValue("host"), port, r.URL.Query().Get("protocol"))
		respond(w, 200, map[string]bool{"released": released}, err)
	})
}

// addPortsTool gives agents the registry. It is a tool of its own because no other fits:
// the app tools manage shartifacts apps, whose backends never need a port from here, and
// this is about machines and services outside shartifacts altogether.
func (s *Server) addPortsTool(m *mcp.Server, agent string) {
	type PortsInput struct {
		Op       string `json:"op,omitempty" jsonschema:"list (default), claim or release"`
		Host     string `json:"host,omitempty" jsonschema:"The machine, as the registry names it: list returns the hosts in use, so reuse a name rather than registering one machine twice. With list, only this host"`
		Port     int    `json:"port,omitempty" jsonschema:"claim: the port, or leave it out for the lowest free one at or above from; release: the port to free"`
		From     int    `json:"from,omitempty" jsonschema:"claim without a port: where to start looking (default 1024)"`
		Protocol string `json:"protocol,omitempty" jsonschema:"tcp (default) or udp"`
		Service  string `json:"service,omitempty" jsonschema:"claim: what listens there, such as the container or compose service"`
		Owner    string `json:"owner,omitempty" jsonschema:"claim: who looks after it (default: this agent's token name)"`
		Note     string `json:"note,omitempty"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "ports", Description: "The network's port registry: which ports services use on each machine here, kept by the agents and people who deploy them. Before publishing a port (a Docker container, a dev server, anything that listens), list the host's claims and claim one; a claim without port takes the lowest free one at or above from, atomically. Claiming again with the same service updates it; a port another service holds is refused with its holder and the next free port. Release a claim when you remove its service. The registry knows only what was claimed, so also check the machine itself when you can (ss -ltnup, docker ps). App backends need none: they listen on $PORT."}, func(ctx context.Context, req *mcp.CallToolRequest, in PortsInput) (*mcp.CallToolResult, any, error) {
		switch in.Op {
		case "", "list":
			list, err := s.Store.Ports(in.Host)
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"claims": list, "hosts": s.Store.PortHosts()}, nil
		case "claim":
			c, created, err := s.claimPort(portClaimInput{Host: in.Host, Port: in.Port, From: in.From, Protocol: in.Protocol, Service: in.Service, Owner: in.Owner, Note: in.Note}, agent)
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"claim": c, "created": created}, nil
		case "release":
			if in.Port == 0 {
				return nil, nil, errors.New("release needs host and port")
			}
			c, released, err := s.Store.ReleasePort(in.Host, in.Port, in.Protocol)
			if err != nil {
				return nil, nil, err
			}
			if !released {
				return nil, map[string]any{"released": false}, nil
			}
			return nil, map[string]any{"released": true, "claim": c}, nil
		}
		return nil, nil, errors.New("op must be list, claim or release")
	})
}

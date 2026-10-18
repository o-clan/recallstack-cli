package cli

import (
	"os"

	"github.com/o-clan/recallstack-cli/internal/remote"
	"github.com/o-clan/recallstack-cli/internal/state"
)

type runtimeServices struct {
	deps Dependencies
	cwd  string
	svc  *remote.Service
}

func newRuntimeServices(deps Dependencies) (*runtimeServices, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &runtimeServices{deps: deps, cwd: cwd, svc: remote.New(deps.Version)}, nil
}

func (r *runtimeServices) person() string { return state.PersonName() }

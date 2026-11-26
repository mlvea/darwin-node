package simvz

import (
	"testing"

	"github.com/mlvea/darwin-node/pkg/vzcontract"
)

func TestSupervisedAdoptContract(t *testing.T) {
	vzcontract.RunSupervisedAdopt(t, simFactory)
}

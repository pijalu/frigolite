package execquery

import (
	"strings"

	"github.com/pijalu/frigolite/internal/function"
	"github.com/pijalu/frigolite/internal/sql"
)

func (e *SelectEngine) isAggregateWindowFunc(fn *sql.FuncCall) bool {
	name := strings.ToUpper(fn.Name)
	if windowOnlyFuncs[name] {
		return false
	}
	reg, found := e.ctx.Functions().Find(fn.Name)
	return found && reg.Type == function.TypeAggregate
}

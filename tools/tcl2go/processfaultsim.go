package main

import (
	"strings"

	tcl "github.com/pijalu/frigolite/tools/tclconvert/tcl"
)

// Fault-injection harness commands (ext/*.test: do_faultsim_test,
// do_malloc_test). The fault injection itself (OOM simulation) is
// unsupported, but their -prep and -body scripts define the schema and data
// later tests rely on. This handler transpiles the -prep script's SQL side
// effects and then skips the assertion machinery (faultsim_save_and_check).
//
// Both commands share the argument layout:
//
//	do_faultsim_test NAME ?-faults PATTERN? -prep SCRIPT ?-body SCRIPT?
func processDoFaultsimTest(tp *transpiler, args []tcl.RawWord) {
	name := "faultsim"
	if len(args) > 0 {
		name = args[0].Text
	}
	tp.emitLine("{ // %s — fault-injection unsupported; prep SQL side effects only", tp.goStringLiteral(tcl.RawWord{Text: name}))
	tp.indent++
	for i := 1; i < len(args); i++ {
		if args[i].Text == "-prep" && i+1 < len(args) && args[i+1].Braced {
			cmds := tcl.ParseCommands(args[i+1].Text)
			tp.processCommands(cmds)
		}
	}
	tp.indent--
	tp.emitLine("}")
}

// processDoMallocTest mirrors the legacy do_malloc_test harness (precursor of
// do_faultsim_test): run the prep script for its side effects only.
func processDoMallocTest(tp *transpiler, args []tcl.RawWord) {
	processDoFaultsimTest(tp, args)
}

// init registers these handlers alongside buildTclCommandHandlers.
func init() {
	if tclCommandHandlers == nil {
		tclCommandHandlers = map[string]tclCmdHandler{}
	}
	tclCommandHandlers["do_faultsim_test"] = processDoFaultsimTest
	tclCommandHandlers["do_malloc_test"] = processDoMallocTest
}

var _ = strings.TrimSpace // placate linters if imports become unused

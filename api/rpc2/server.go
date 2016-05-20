package rpc2

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	grpc "net/rpc"
	"net/rpc/jsonrpc"

	"github.com/derekparker/delve/api"
	"github.com/derekparker/delve/api/debugger"
	"github.com/derekparker/delve/api/types"
)

type ServerImpl struct {
	s *RPCServer
}

type RPCServer struct {
	// config is all the information necessary to start the debugger and server.
	config *api.Config
	// listener is used to serve HTTP.
	listener net.Listener
	// stopChan is used to stop the listener goroutine
	stopChan chan struct{}
	// debugger is a debugger service.
	debugger *debugger.Debugger
}

// NewServer creates a new RPCServer.
func NewServer(config *api.Config, logEnabled bool) *ServerImpl {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	if !logEnabled {
		log.SetOutput(ioutil.Discard)
	}

	return &ServerImpl{
		&RPCServer{
			config:   config,
			listener: config.Listener,
			stopChan: make(chan struct{}),
		},
	}
}

// Stop detaches from the debugger and waits for it to stop.
func (s *ServerImpl) Stop(kill bool) error {
	if s.s.config.AcceptMulti {
		close(s.s.stopChan)
		s.s.listener.Close()
	}
	err := s.s.debugger.Detach(kill)
	if err != nil {
		return err
	}
	return nil
}

// Run starts a debugger and exposes it with an HTTP server. The debugger
// itself can be stopped with the `detach` API. Run blocks until the HTTP
// server stops.
func (s *ServerImpl) Run() error {
	var err error
	// Create and start the debugger
	if s.s.debugger, err = debugger.New(&debugger.Config{
		ProcessArgs: s.s.config.ProcessArgs,
		AttachPid:   s.s.config.AttachPid,
	}); err != nil {
		return err
	}

	rpcs := grpc.NewServer()
	rpcs.Register(s.s)

	go func() {
		defer s.s.listener.Close()
		for {
			c, err := s.s.listener.Accept()
			if err != nil {
				select {
				case <-s.s.stopChan:
					// We were supposed to exit, do nothing and return
					return
				default:
					panic(err)
				}
			}
			go rpcs.ServeCodec(jsonrpc.NewServerCodec(c))
			if !s.s.config.AcceptMulti {
				break
			}
		}
	}()
	return nil
}

func (s *ServerImpl) Restart() error {
	return s.s.Restart(RestartIn{}, nil)
}

type ProcessPidIn struct {
}

type ProcessPidOut struct {
	Pid int
}

// ProcessPid returns the pid of the process we are debugging.
func (s *RPCServer) ProcessPid(arg ProcessPidIn, out *ProcessPidOut) error {
	out.Pid = s.debugger.ProcessPid()
	return nil
}

type DetachIn struct {
	Kill bool
}

type DetachOut struct {
}

// Detach detaches the debugger, optionally killing the process.
func (s *RPCServer) Detach(arg DetachIn, out *DetachOut) error {
	return s.debugger.Detach(arg.Kill)
}

type RestartIn struct {
}

type RestartOut struct {
}

// Restart restarts program.
func (s *RPCServer) Restart(arg RestartIn, out *RestartOut) error {
	if s.config.AttachPid != 0 {
		return errors.New("cannot restart process Delve did not create")
	}
	return s.debugger.Restart()
}

type StateIn struct {
}

type StateOut struct {
	State *types.DebuggerState
}

// State returns the current debugger state.
func (s *RPCServer) State(arg StateIn, out *StateOut) error {
	st, err := s.debugger.State()
	if err != nil {
		return err
	}
	out.State = st
	return nil
}

type CommandOut struct {
	State types.DebuggerState
}

// Command interrupts, continues and steps through the program.
func (s *RPCServer) Command(command types.DebuggerCommand, out *CommandOut) error {
	st, err := s.debugger.Command(&command)
	if err != nil {
		return err
	}
	out.State = *st
	return nil
}

type GetBreakpointIn struct {
	Id   int
	Name string
}

type GetBreakpointOut struct {
	Breakpoint types.Breakpoint
}

// GetBreakpoint gets a breakpoint by Name (if Name is not an empty string) or by ID.
func (s *RPCServer) GetBreakpoint(arg GetBreakpointIn, out *GetBreakpointOut) error {
	var bp *types.Breakpoint
	if arg.Name != "" {
		bp = s.debugger.FindBreakpointByName(arg.Name)
		if bp == nil {
			return fmt.Errorf("no breakpoint with name %s", arg.Name)
		}
	} else {
		bp = s.debugger.FindBreakpoint(arg.Id)
		if bp == nil {
			return fmt.Errorf("no breakpoint with id %d", arg.Id)
		}
	}
	out.Breakpoint = *bp
	return nil
}

type StacktraceIn struct {
	Id    int
	Depth int
	Full  bool
	Cfg   *types.LoadConfig
}

type StacktraceOut struct {
	Locations []types.Stackframe
}

// Stacktrace returns stacktrace of goroutine Id up to the specified Depth.
//
// If Full is set it will also the variable of all local variables
// and function arguments of all stack frames.
func (s *RPCServer) Stacktrace(arg StacktraceIn, out *StacktraceOut) error {
	cfg := arg.Cfg
	if cfg == nil && arg.Full {
		cfg = &types.LoadConfig{true, 1, 64, 64, -1}
	}
	locs, err := s.debugger.Stacktrace(arg.Id, arg.Depth, types.LoadConfigToProc(cfg))
	if err != nil {
		return err
	}
	out.Locations = locs
	return nil
}

type ListBreakpointsIn struct {
}

type ListBreakpointsOut struct {
	Breakpoints []*types.Breakpoint
}

// ListBreakpoints gets all breakpoints.
func (s *RPCServer) ListBreakpoints(arg ListBreakpointsIn, out *ListBreakpointsOut) error {
	out.Breakpoints = s.debugger.Breakpoints()
	return nil
}

type CreateBreakpointIn struct {
	Breakpoint types.Breakpoint
}

type CreateBreakpointOut struct {
	Breakpoint types.Breakpoint
}

// CreateBreakpoint creates a new breakpoint.
//
// - If arg.Breakpoint.File is not an empty string the breakpoint
// will be created on the specified file:line location
//
// - If arg.Breakpoint.FunctionName is not an empty string
// the breakpoint will be created on the specified function:line
// location. Note that setting a breakpoint on a function's entry point
// (line == 0) can have surprising consequences, it is advisable to
// use line = -1 instead which will skip the prologue.
//
// - Otherwise the value specified by arg.Breakpoint.Addr will be used.
func (s *RPCServer) CreateBreakpoint(arg CreateBreakpointIn, out *CreateBreakpointOut) error {
	createdbp, err := s.debugger.CreateBreakpoint(&arg.Breakpoint)
	if err != nil {
		return err
	}
	out.Breakpoint = *createdbp
	return nil
}

type ClearBreakpointIn struct {
	Id   int
	Name string
}

type ClearBreakpointOut struct {
	Breakpoint *types.Breakpoint
}

// ClearBreakpoint deletes a breakpoint by Name (if Name is not an
// empty string) or by ID.
func (s *RPCServer) ClearBreakpoint(arg ClearBreakpointIn, out *ClearBreakpointOut) error {
	var bp *types.Breakpoint
	if arg.Name != "" {
		bp = s.debugger.FindBreakpointByName(arg.Name)
		if bp == nil {
			return fmt.Errorf("no breakpoint with name %s", arg.Name)
		}
	} else {
		bp = s.debugger.FindBreakpoint(arg.Id)
		if bp == nil {
			return fmt.Errorf("no breakpoint with id %d", arg.Id)
		}
	}
	deleted, err := s.debugger.ClearBreakpoint(bp)
	if err != nil {
		return err
	}
	out.Breakpoint = deleted
	return nil
}

type AmendBreakpointIn struct {
	Breakpoint types.Breakpoint
}

type AmendBreakpointOut struct {
}

// AmendBreakpoint allows user to update an existing breakpoint
// for example to change the information retrieved when the
// breakpoint is hit or to change, add or remove the break condition.
//
// arg.Breakpoint.ID must be a valid breakpoint ID
func (s *RPCServer) AmendBreakpoint(arg AmendBreakpointIn, out *AmendBreakpointOut) error {
	return s.debugger.AmendBreakpoint(&arg.Breakpoint)
}

type CancelNextIn struct {
}

type CancelNextOut struct {
}

func (s *RPCServer) CancelNext(arg CancelNextIn, out *CancelNextOut) error {
	return s.debugger.CancelNext()
}

type ListThreadsIn struct {
}

type ListThreadsOut struct {
	Threads []*types.Thread
}

// ListThreads lists all threads.
func (s *RPCServer) ListThreads(arg ListThreadsIn, out *ListThreadsOut) (err error) {
	out.Threads, err = s.debugger.Threads()
	return err
}

type GetThreadIn struct {
	Id int
}

type GetThreadOut struct {
	Thread *types.Thread
}

// GetThread gets a thread by its ID.
func (s *RPCServer) GetThread(arg GetThreadIn, out *GetThreadOut) error {
	t, err := s.debugger.FindThread(arg.Id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("no thread with id %d", arg.Id)
	}
	out.Thread = t
	return nil
}

type ListPackageVarsIn struct {
	Filter string
	Cfg    types.LoadConfig
}

type ListPackageVarsOut struct {
	Variables []types.Variable
}

// ListPackageVars lists all package variables in the context of the current thread.
func (s *RPCServer) ListPackageVars(arg ListPackageVarsIn, out *ListPackageVarsOut) error {
	state, err := s.debugger.State()
	if err != nil {
		return err
	}

	current := state.CurrentThread
	if current == nil {
		return fmt.Errorf("no current thread")
	}

	vars, err := s.debugger.PackageVariables(current.ID, arg.Filter, *types.LoadConfigToProc(&arg.Cfg))
	if err != nil {
		return err
	}
	out.Variables = vars
	return nil
}

type ListRegistersIn struct {
}

type ListRegistersOut struct {
	Registers string
}

// ListRegisters lists registers and their values.
func (s *RPCServer) ListRegisters(arg ListRegistersIn, out *ListRegistersOut) error {
	state, err := s.debugger.State()
	if err != nil {
		return err
	}

	regs, err := s.debugger.Registers(state.CurrentThread.ID)
	if err != nil {
		return err
	}
	out.Registers = regs
	return nil
}

type ListLocalVarsIn struct {
	Scope types.EvalScope
	Cfg   types.LoadConfig
}

type ListLocalVarsOut struct {
	Variables []types.Variable
}

// ListLocalVars lists all local variables in scope.
func (s *RPCServer) ListLocalVars(arg ListLocalVarsIn, out *ListLocalVarsOut) error {
	vars, err := s.debugger.LocalVariables(arg.Scope, *types.LoadConfigToProc(&arg.Cfg))
	if err != nil {
		return err
	}
	out.Variables = vars
	return nil
}

type ListFunctionArgsIn struct {
	Scope types.EvalScope
	Cfg   types.LoadConfig
}

type ListFunctionArgsOut struct {
	Args []types.Variable
}

// ListFunctionArgs lists all arguments to the current function
func (s *RPCServer) ListFunctionArgs(arg ListFunctionArgsIn, out *ListFunctionArgsOut) error {
	vars, err := s.debugger.FunctionArguments(arg.Scope, *types.LoadConfigToProc(&arg.Cfg))
	if err != nil {
		return err
	}
	out.Args = vars
	return nil
}

type EvalIn struct {
	Scope types.EvalScope
	Expr  string
	Cfg   *types.LoadConfig
}

type EvalOut struct {
	Variable *types.Variable
}

// EvalVariable returns a variable in the specified context.
//
// See https://github.com/derekparker/delve/wiki/Expressions for
// a description of acceptable values of arg.Expr.
func (s *RPCServer) Eval(arg EvalIn, out *EvalOut) error {
	cfg := arg.Cfg
	if cfg == nil {
		cfg = &types.LoadConfig{true, 1, 64, 64, -1}
	}
	v, err := s.debugger.EvalVariableInScope(arg.Scope, arg.Expr, *types.LoadConfigToProc(cfg))
	if err != nil {
		return err
	}
	out.Variable = v
	return nil
}

type SetIn struct {
	Scope  types.EvalScope
	Symbol string
	Value  string
}

type SetOut struct {
}

// Set sets the value of a variable. Only numerical types and
// pointers are currently supported.
func (s *RPCServer) Set(arg SetIn, out *SetOut) error {
	return s.debugger.SetVariableInScope(arg.Scope, arg.Symbol, arg.Value)
}

type ListSourcesIn struct {
	Filter string
}

type ListSourcesOut struct {
	Sources []string
}

// ListSources lists all source files in the process matching filter.
func (s *RPCServer) ListSources(arg ListSourcesIn, out *ListSourcesOut) error {
	ss, err := s.debugger.Sources(arg.Filter)
	if err != nil {
		return err
	}
	out.Sources = ss
	return nil
}

type ListFunctionsIn struct {
	Filter string
}

type ListFunctionsOut struct {
	Funcs []string
}

// ListFunctions lists all functions in the process matching filter.
func (s *RPCServer) ListFunctions(arg ListFunctionsIn, out *ListFunctionsOut) error {
	fns, err := s.debugger.Functions(arg.Filter)
	if err != nil {
		return err
	}
	out.Funcs = fns
	return nil
}

type ListTypesIn struct {
	Filter string
}

type ListTypesOut struct {
	Types []string
}

// ListTypes lists all types in the process matching filter.
func (s *RPCServer) ListTypes(arg ListTypesIn, out *ListTypesOut) error {
	tps, err := s.debugger.Types(arg.Filter)
	if err != nil {
		return err
	}
	out.Types = tps
	return nil
}

type ListGoroutinesIn struct {
}

type ListGoroutinesOut struct {
	Goroutines []*types.Goroutine
}

// ListGoroutines lists all goroutines.
func (s *RPCServer) ListGoroutines(arg ListGoroutinesIn, out *ListGoroutinesOut) error {
	gs, err := s.debugger.Goroutines()
	if err != nil {
		return err
	}
	out.Goroutines = gs
	return nil
}

type AttachedToExistingProcessIn struct {
}

type AttachedToExistingProcessOut struct {
	Answer bool
}

// AttachedToExistingProcess returns whether we attached to a running process or not
func (c *RPCServer) AttachedToExistingProcess(arg AttachedToExistingProcessIn, out *AttachedToExistingProcessOut) error {
	if c.config.AttachPid != 0 {
		out.Answer = true
	}
	return nil
}

type FindLocationIn struct {
	Scope types.EvalScope
	Loc   string
}

type FindLocationOut struct {
	Locations []types.Location
}

// FindLocation returns concrete location information described by a location expression
//
//  loc ::= <filename>:<line> | <function>[:<line>] | /<regex>/ | (+|-)<offset> | <line> | *<address>
//  * <filename> can be the full path of a file or just a suffix
//  * <function> ::= <package>.<receiver type>.<name> | <package>.(*<receiver type>).<name> | <receiver type>.<name> | <package>.<name> | (*<receiver type>).<name> | <name>
//  * <function> must be unambiguous
//  * /<regex>/ will return a location for each function matched by regex
//  * +<offset> returns a location for the line that is <offset> lines after the current line
//  * -<offset> returns a location for the line that is <offset> lines before the current line
//  * <line> returns a location for a line in the current file
//  * *<address> returns the location corresponding to the specified address
//
// NOTE: this function does not actually set breakpoints.
func (c *RPCServer) FindLocation(arg FindLocationIn, out *FindLocationOut) error {
	var err error
	out.Locations, err = c.debugger.FindLocation(arg.Scope, arg.Loc)
	return err
}

type DisassembleIn struct {
	Scope          types.EvalScope
	StartPC, EndPC uint64
	Flavour        types.AssemblyFlavour
}

type DisassembleOut struct {
	Disassemble types.AsmInstructions
}

// Disassemble code.
//
// If both StartPC and EndPC are non-zero the specified range will be disassembled, otherwise the function containing StartPC will be disassembled.
//
// Scope is used to mark the instruction the specified gorutine is stopped at.
//
// Disassemble will also try to calculate the destination address of an absolute indirect CALL if it happens to be the instruction the selected goroutine is stopped at.
func (c *RPCServer) Disassemble(arg DisassembleIn, out *DisassembleOut) error {
	var err error
	out.Disassemble, err = c.debugger.Disassemble(arg.Scope, arg.StartPC, arg.EndPC, arg.Flavour)
	return err
}
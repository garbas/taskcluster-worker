package nativeengine

import (
	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/taskcluster/taskcluster-worker/engines"
	"github.com/taskcluster/taskcluster-worker/engines/native/system"
	"github.com/taskcluster/taskcluster-worker/runtime"
	"github.com/taskcluster/taskcluster-worker/runtime/atomics"
	"github.com/taskcluster/taskcluster-worker/runtime/ioext"
)

type sandbox struct {
	engines.SandboxBase
	engine     *engine
	context    *runtime.TaskContext
	log        *logrus.Entry
	homeFolder runtime.TemporaryFolder
	user       *system.User
	process    *system.Process
	env        map[string]string
	resolve    atomics.Once // Guarding resultSet, resultErr and abortErr
	resultSet  *resultSet
	resultErr  error
	abortErr   error
	wg         atomics.WaitGroup
}

func newSandbox(b *sandboxBuilder) (*sandbox, error) {
	// Create temporary home folder for the task
	homeFolder, err := b.engine.environment.TemporaryStorage.NewFolder()
	if err != nil {
		b.log.Error("Failed to create temporary folder: ", err)
		return nil, fmt.Errorf("Failed to temporary folder, error: %s", err)
	}

	// Create temporary user account
	user, err := system.CreateUser(homeFolder.Path(), b.engine.groups)
	if err != nil {
		homeFolder.Remove() // best-effort clean-up this is a fatal error
		return nil, fmt.Errorf("Failed to create temporary system user, error: %s", err)
	}

	// Start process
	debug("StartProcess: %v", b.payload.Command)
	process, err := system.StartProcess(system.ProcessOptions{
		Arguments:     b.payload.Command,
		Environment:   b.env,
		WorkingFolder: homeFolder.Path(),
		Owner:         user,
		Stdout:        ioext.WriteNopCloser(b.context.LogDrain()),
		// Stderr defaults to Stdout when not specified
	})
	if err != nil {
		// StartProcess provides human-readable error messages (see docs)
		// We'll convert it to a MalformedPayloadError
		return nil, engines.NewMalformedPayloadError(
			"Unable to start specified command: %v, error: %s",
			b.payload.Command, err,
		)
	}

	s := &sandbox{
		engine:     b.engine,
		context:    b.context,
		log:        b.log,
		homeFolder: homeFolder,
		user:       user,
		process:    process,
		env:        b.env,
	}

	go s.waitForTermination()

	return s, nil
}

func (s *sandbox) NewShell(command []string, tty bool) (engines.Shell, error) {
	// Increment shell counter, if draining we don't allow new shells
	if s.wg.Add(1) != nil {
		return nil, engines.ErrSandboxTerminated
	}

	debug("NewShell with: %v", command)
	shell, err := newShell(s, command, tty)
	if err != nil {
		debug("Failed to start shell, error: %s", err)
		s.wg.Done()
		return nil, engines.NewMalformedPayloadError(
			"Unable to spawn command: ", command, " error: ", err,
		)
	}

	// Wait for the shell to be done and decrement WaitGroup
	go func() {
		result, _ := shell.Wait()
		debug("Shell finished with: %v", result)
		s.wg.Done()
	}()

	return shell, nil
}

func (s *sandbox) waitForTermination() {
	// Wait for process to terminate
	success := s.process.Wait()
	debug("Process finished with: %v", success)

	// Wait for all shell to finish and prevent new shells from being created
	s.wg.WaitAndDrain()
	debug("All shells terminated")

	s.resolve.Do(func() {
		// Halt all other sub-processes
		system.KillByOwner(s.user)

		// Create resultSet
		s.resultSet = &resultSet{
			engine:     s.engine,
			context:    s.context,
			log:        s.log,
			homeFolder: s.homeFolder,
			user:       s.user,
			success:    success,
		}
		s.abortErr = engines.ErrSandboxTerminated
	})
}

func (s *sandbox) WaitForResult() (engines.ResultSet, error) {
	// Wait for result and terminate
	s.resolve.Wait()
	return s.resultSet, s.resultErr
}

func (s *sandbox) Abort() error {
	s.resolve.Do(func() {
		debug("Aborting sandbox")

		// Kill sub-process
		s.process.Kill()

		// Halt all other sub-processes
		err := system.KillByOwner(s.user)
		if err != nil {
			s.log.Error("Failed to kill all processes by owner, error: ", err)
		}

		// Remove temporary user (this will panic if unsuccessful)
		s.user.Remove()

		// Remove temporary home folder
		err = s.homeFolder.Remove()
		if err != nil {
			s.log.Error("Failed to remove temporary home directory, error: ", err)
		}

		// Set result
		s.resultErr = engines.ErrSandboxAborted
	})
	s.resolve.Wait()
	return s.abortErr
}
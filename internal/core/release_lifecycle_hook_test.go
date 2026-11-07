package core

// watchLifecycle installs f as m's lifecycle hook (Manager.lifecycleHook)
// before m attaches anything.
func watchLifecycle(m *Manager, f func(ev lifecycleEvent)) { m.lifecycleHook = f }

package runner

func (d *drive) cleanup() (flow, error) { return d.stop(DriveCleaning) }

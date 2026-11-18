package provision

// Options controls a provisioning run.
type Options struct {
	PreflightOnly bool
	RequireRoot   bool
	// NewClient opens the admin connection (tests may inject a fake).
	NewClient func(*AdminConnection) Client
}

// Run mirrors run(): it returns whether anything changed.
func Run(configPath, runtimePath, schemaPath, journalPath string, opts Options) (bool, error) {
	if opts.RequireRoot && geteuid() != 0 {
		return false, provisionErr("dataset provisioning must run as root")
	}
	config, err := SecureReadJSON(configPath, opts.RequireRoot)
	if err != nil {
		return false, err
	}
	runtime, err := SecureReadJSON(runtimePath, false)
	if err != nil {
		return false, err
	}
	admin, datasets, err := ValidateRegistry(config)
	if err != nil {
		return false, err
	}
	mainDatabase := config.Lookup("main_database").(string)
	runtimeByID, _, err := ValidateRuntime(runtime, mainDatabase)
	if err != nil {
		return false, err
	}
	schema, err := ParseCanonicalSchema(pyPath(schemaPath))
	if err != nil {
		return false, err
	}
	journal, err := ReadJournal(journalPath, opts.RequireRoot)
	if err != nil {
		return false, err
	}
	var client Client
	if opts.NewClient != nil {
		client = opts.NewClient(admin)
	} else {
		client = NewHTTPClient(admin)
	}
	p := NewProvisioner(client, admin, schema, journalPath, journal, opts.RequireRoot, config.Lookup("main_user").(string))
	if err := p.Preflight(datasets, runtimeByID); err != nil {
		return false, err
	}
	if opts.PreflightOnly {
		return false, nil
	}
	if err := p.Provision(datasets, runtimeByID); err != nil {
		return false, err
	}
	return p.Changed, nil
}

package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/luckoseabraham/opa-driver/pkg/drivers"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/storage"
	"github.com/open-policy-agent/opa/storage/inmem"
	"github.com/open-policy-agent/opa/topdown"
	"github.com/pkg/errors"
)

type module struct {
	text   string
	parsed *ast.Module
}

type insertParam map[string]*module

func (i insertParam) add(name string, src string) error {
	m, err := ast.ParseModule(name, src)
	if err != nil {
		return err
	}
	i[name] = &module{text: src, parsed: m}
	return nil
}

type Arg func(*driver)

func Tracing(enabled bool) Arg {
	return func(d *driver) {
		d.traceEnabled = enabled
	}
}

func New(args ...Arg) drivers.Driver {
	d := &driver{
		compiler: ast.NewCompiler(),
		modules:  make(map[string]*ast.Module),
		storage:  inmem.New(),
	}
	for _, arg := range args {
		arg(d)
	}
	return d
}

var _ drivers.Driver = &driver{}

type driver struct {
	modulesMux   sync.RWMutex
	compiler     *ast.Compiler
	modules      map[string]*ast.Module
	storage      storage.Store
	traceEnabled bool
}

func (d *driver) Init(ctx context.Context) error {
	return nil
}

func copyModules(modules map[string]*ast.Module, filter string) map[string]*ast.Module {
	m := make(map[string]*ast.Module, len(modules))
	for k, v := range modules {
		if filter != "" && k == filter {
			continue
		}
		m[k] = v
	}
	return m
}

func (d *driver) checkModuleName(name string) error {
	if name == "" {
		return errors.Errorf("Module name cannot be empty")
	}
	return nil
}

func (d *driver) PutModule(ctx context.Context, name string, src string) error {
	if err := d.checkModuleName(name); err != nil {
		return err
	}
	insert := insertParam{}
	if err := insert.add(name, src); err != nil {
		return err
	}
	d.modulesMux.Lock()
	defer d.modulesMux.Unlock()
	_, err := d.alterModules(ctx, insert, nil)
	return err
}

// DeleteModule deletes a rule from OPA and returns true if a rule was found and deleted, false
// if a rule was not found, and any errors
func (d *driver) DeleteModule(ctx context.Context, name string) (bool, error) {
	if err := d.checkModuleName(name); err != nil {
		return false, err
	}
	d.modulesMux.Lock()
	defer d.modulesMux.Unlock()
	if _, found := d.modules[name]; !found {
		return false, nil
	}
	count, err := d.alterModules(ctx, nil, []string{name})
	return count == 1, err
}

// alterModules alters the modules in the driver by inserting and removing
// the provided modules then returns the count of modules removed.
// alterModules expects that the caller is holding the modulesMux lock.
func (d *driver) alterModules(ctx context.Context, insert insertParam, remove []string) (int, error) {
	updatedModules := copyModules(d.modules, "")
	for _, name := range remove {
		delete(updatedModules, name)
	}
	for name, mod := range insert {
		updatedModules[name] = mod.parsed
	}

	txn, err := d.storage.NewTransaction(ctx, storage.WriteParams)
	if err != nil {
		return 0, err
	}

	for _, name := range remove {
		if err := d.storage.DeletePolicy(ctx, txn, name); err != nil {
			d.storage.Abort(ctx, txn)
			return 0, err
		}
	}

	c := ast.NewCompiler().WithPathConflictsCheck(storage.NonEmpty(ctx, d.storage, txn))
	if c.Compile(updatedModules); c.Failed() {
		d.storage.Abort(ctx, txn)
		return 0, c.Errors
	}

	for name, mod := range insert {
		if err := d.storage.UpsertPolicy(ctx, txn, name, []byte(mod.text)); err != nil {
			d.storage.Abort(ctx, txn)
			return 0, err
		}
	}
	if err := d.storage.Commit(ctx, txn); err != nil {
		return 0, err
	}
	d.compiler = c
	d.modules = updatedModules
	return len(remove), nil
}

func parsePath(path string) ([]string, error) {
	p, ok := storage.ParsePathEscaped(path)
	if !ok {
		return nil, fmt.Errorf("Bad data path: %s", path)
	}
	return p, nil
}

func (d *driver) PutData(ctx context.Context, path string, data interface{}) error {
	d.modulesMux.RLock()
	defer d.modulesMux.RUnlock()
	p, err := parsePath(path)
	if err != nil {
		return err
	}
	txn, err := d.storage.NewTransaction(ctx, storage.WriteParams)
	if err != nil {
		return err
	}
	if _, err := d.storage.Read(ctx, txn, p); err != nil {
		if storage.IsNotFound(err) {
			if err := storage.MakeDir(ctx, d.storage, txn, p[:len(p)-1]); err != nil {
				return err
			}
		} else {
			d.storage.Abort(ctx, txn)
			return err
		}
	}
	if err := d.storage.Write(ctx, txn, storage.AddOp, p, data); err != nil {
		d.storage.Abort(ctx, txn)
		return err
	}
	if err := ast.CheckPathConflicts(d.compiler, storage.NonEmpty(ctx, d.storage, txn)); len(err) > 0 {
		d.storage.Abort(ctx, txn)
		return err
	}
	if err := d.storage.Commit(ctx, txn); err != nil {
		return err
	}
	return nil
}

// DeleteData deletes data from OPA and returns true if data was found and deleted, false
// if data was not found, and any errors
func (d *driver) DeleteData(ctx context.Context, path string) (bool, error) {
	d.modulesMux.RLock()
	defer d.modulesMux.RUnlock()
	p, err := parsePath(path)
	if err != nil {
		return false, err
	}
	txn, err := d.storage.NewTransaction(ctx, storage.WriteParams)
	if err != nil {
		return false, err
	}
	if err := d.storage.Write(ctx, txn, storage.RemoveOp, p, interface{}(nil)); err != nil {
		d.storage.Abort(ctx, txn)
		if storage.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := d.storage.Commit(ctx, txn); err != nil {
		return false, err
	}
	return true, nil
}

func (d *driver) eval(ctx context.Context, path string, input interface{}, cfg *drivers.QueryCfg) (rego.ResultSet, *string, error) {
	d.modulesMux.RLock()
	defer d.modulesMux.RUnlock()
	args := []func(*rego.Rego){
		rego.Compiler(d.compiler),
		rego.Store(d.storage),
		rego.Input(input),
		rego.Query(path),
	}
	if d.traceEnabled || cfg.TracingEnabled {
		buf := topdown.NewBufferTracer()
		args = append(args, rego.Tracer(buf))
		rego := rego.New(args...)
		res, err := rego.Eval(ctx)
		b := &bytes.Buffer{}
		topdown.PrettyTrace(b, *buf)
		t := b.String()
		return res, &t, err
	}
	rego := rego.New(args...)
	res, err := rego.Eval(ctx)
	return res, nil, err
}

func (d *driver) Query(ctx context.Context, path string, input interface{}, opts ...drivers.QueryOpt) (*drivers.Response, error) {
	cfg := &drivers.QueryCfg{}
	for _, opt := range opts {
		opt(cfg)
	}
	inp, err := json.MarshalIndent(input, "", "   ")
	if err != nil {
		return nil, err
	}
	// Add a variable binding to the path
	rs, trace, err := d.eval(ctx, path, input, cfg)
	if err != nil {
		return nil, err
	}
	i := string(inp)
	return &drivers.Response{
		Trace:   trace,
		Results: rs,
		Input:   &i,
	}, nil
}

func (d *driver) Dump(ctx context.Context) (string, error) {
	d.modulesMux.RLock()
	defer d.modulesMux.RUnlock()
	mods := make(map[string]string, len(d.modules))
	for k, v := range d.modules {
		mods[k] = v.String()
	}
	data, _, err := d.eval(ctx, "data", nil, &drivers.QueryCfg{})
	if err != nil {
		return "", err
	}
	var dt interface{}
	// There should be only 1 or 0 expression values
	if len(data) > 1 {
		return "", errors.New("Too many dump results")
	}
	for _, da := range data {
		if len(data) > 1 {
			return "", errors.New("Too many expressions results")
		}
		for _, e := range da.Expressions {
			dt = e.Value
		}
	}
	resp := map[string]interface{}{
		"modules": mods,
		"data":    dt,
	}
	b, err := json.MarshalIndent(resp, "", "   ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

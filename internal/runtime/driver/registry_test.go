package driver

import (
	"context"
	"testing"

	"github.com/canter0/canter/sdk"
)

type fakeDriver struct{}

func (fakeDriver) Ensure(context.Context, sdk.RuntimeService) (Result, error) {
	return Result{URL: "scheme://private", Endpoint: "private:1234"}, nil
}

func TestRegistryCreatesGenericServiceBinding(t *testing.T) {
	registry := NewRegistry()
	registry.Register("database", "example", fakeDriver{})
	bindings, observed, err := registry.Ensure(context.Background(), sdk.RuntimePlan{Services: []sdk.RuntimeService{{Name: "primary-data", Kind: "database", Engine: "example"}}})
	if err != nil {
		t.Fatal(err)
	}
	if bindings["CANTER_SERVICE_PRIMARY_DATA_URL"] != "scheme://private" {
		t.Fatalf("bindings=%v", bindings)
	}
	if len(observed) != 1 || observed[0].Endpoint != "private:1234" || observed[0].Phase != "ready" {
		t.Fatalf("observed=%+v", observed)
	}
}

func TestPostgresCredentialsAreStableAndPrivate(t *testing.T) {
	driver := Postgres{Root: t.TempDir()}
	first, err := driver.credentials("database")
	if err != nil {
		t.Fatal(err)
	}
	second, err := driver.credentials("database")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.Password == "" {
		t.Fatalf("credentials did not persist: first=%+v second=%+v", first, second)
	}
}

package pivot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// errBoom is an API error that is neither NotFound nor NoMatch.
var errBoom = errors.New("boom")

// noMatch is what the API returns for a kind whose CRD is not installed.
func noMatch(kind string) error {
	return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "example.com", Kind: kind}}
}

// fault fails one verb on one kind (and, if set, one name) with err, after
// letting skip matching calls through.
type fault struct {
	verb, kind, name string
	skip             int
	err              error
	calls            int
}

func (f *fault) hit(verb, kind, name string) error {
	if f.verb != verb || f.kind != kind || (f.name != "" && f.name != name) {
		return nil
	}
	f.calls++
	if f.calls <= f.skip {
		return nil
	}
	return f.err
}

func kindOf(c client.WithWatch, obj runtime.Object) string {
	gvk, err := apiutil.GVKForObject(obj, c.Scheme())
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(gvk.Kind, "List")
}

// faulty wraps c so that the given faults fire; every other call goes
// through to c.
func faulty(t *testing.T, c client.Client, faults ...*fault) client.Client {
	t.Helper()
	check := func(cl client.WithWatch, verb string, obj runtime.Object, name string) error {
		k := kindOf(cl, obj)
		for _, f := range faults {
			if err := f.hit(verb, k, name); err != nil {
				return err
			}
		}
		return nil
	}
	return interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := check(cl, "get", obj, key.Name); err != nil {
				return err
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := check(cl, "list", list, ""); err != nil {
				return err
			}
			return cl.List(ctx, list, opts...)
		},
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := check(cl, "create", obj, obj.GetName()); err != nil {
				return err
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := check(cl, "update", obj, obj.GetName()); err != nil {
				return err
			}
			return cl.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := check(cl, "patch", obj, obj.GetName()); err != nil {
				return err
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := check(cl, "delete", obj, obj.GetName()); err != nil {
				return err
			}
			return cl.Delete(ctx, obj, opts...)
		},
	})
}

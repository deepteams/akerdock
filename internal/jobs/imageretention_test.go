package jobs

import (
	"reflect"
	"testing"
)

func TestImagesToReclaim(t *testing.T) {
	art := func(id int64, ref string) prunableImage { return prunableImage{id: id, ref: ref} }

	t.Run("keeps everything within the retention window", func(t *testing.T) {
		arts := []prunableImage{art(5, "app:e"), art(4, "app:d"), art(3, "app:c")}
		if got := imagesToReclaim(arts, 5); got != nil {
			t.Fatalf("expected nothing to reclaim, got %v", got)
		}
	})

	t.Run("reclaims the oldest beyond N, newest-first order preserved", func(t *testing.T) {
		arts := []prunableImage{
			art(7, "app:g"), art(6, "app:f"), art(5, "app:e"),
			art(4, "app:d"), art(3, "app:c"), art(2, "app:b"), art(1, "app:a"),
		}
		got := imagesToReclaim(arts, 5)
		want := []prunableImage{art(2, "app:b"), art(1, "app:a")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("never reclaims an image a kept artifact still references", func(t *testing.T) {
		// A constant registry tag reused across deployments: the old pointer must
		// be dropped (blank ref) without rmi-ing the image the live one uses.
		arts := []prunableImage{
			art(9, "app:latest"), art(8, "app:v2"), art(7, "app:v1"),
			art(6, "app:latest"),
		}
		got := imagesToReclaim(arts, 3)
		want := []prunableImage{{id: 6, ref: ""}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("a retention below one still protects the live image", func(t *testing.T) {
		arts := []prunableImage{art(2, "app:new"), art(1, "app:old")}
		got := imagesToReclaim(arts, 0)
		want := []prunableImage{art(1, "app:old")}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

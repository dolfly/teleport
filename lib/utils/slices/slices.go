/*
 * Teleport
 * Copyright (C) 2024  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package slices

import (
	"slices"

	"github.com/gravitational/trace"
)

// FilterMapUnique applies a function to all elements of a slice and collects them.
// The function returns the value to collect and whether the current element should be included.
// Returned values are deduplicated.
func FilterMapUnique[T any, S comparable](ts []T, fn func(T) (s S, include bool)) []S {
	ss := make([]S, 0, len(ts))
	seen := make(map[S]struct{}, len(ts))
	for _, t := range ts {
		if s, include := fn(t); include {
			if _, found := seen[s]; !found {
				seen[s] = struct{}{}
				ss = append(ss, s)
			}
		}
	}

	return ss
}

// Map calls the provided function on each element of a slice, and returns a slice containin the results.
func Map[T, S any](items []T, fn func(T) S) []S {
	result := make([]S, len(items))
	for i, item := range items {
		result[i] = fn(item)
	}
	return result
}

// ToPointers converts a slice of values to a slice of pointers to those values
func ToPointers[T any](in []T) []*T {
	out := make([]*T, len(in))
	for i := range in {
		out[i] = &in[i]
	}
	return out
}

// ContainsAll checks whether haystack contains all needles, reporting
// error indicating the first unmatched value.
func ContainsAll[S ~[]E, E comparable](haystack S, needles S) error {
	for _, needle := range needles {
		if !slices.Contains(haystack, needle) {
			return trace.BadParameter("%v not in set", needle)
		}
	}
	return nil
}

// FromPointers converts a slice of pointers to values to a slice of values.
// Nil pointers are converted to zero-values.
func FromPointers[T any](in []*T) []T {
	out := make([]T, len(in))
	for i := range in {
		if in[i] == nil {
			continue
		}
		out[i] = *in[i]
	}
	return out
}

// DeduplicateKey returns a deduplicated slice by comparing key values from the key function
func DeduplicateKey[T any](s []T, key func(T) string) []T {
	out := make([]T, 0, len(s))
	seen := make(map[string]struct{})
	for _, v := range s {
		if _, ok := seen[key(v)]; ok {
			continue
		}
		seen[key(v)] = struct{}{}
		out = append(out, v)
	}
	return out
}

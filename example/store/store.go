package store

import (
	"maps"
	"slices"
	"sync"
)

type UserInput struct {
	Name  string `json:"name" minLength:"1" maxLength:"64" doc:"Display name" example:"Ada"`
	Email string `json:"email" format:"email" example:"ada@example.com"`
	Role  string `json:"role" enum:"admin,member" default:"member"`
}

type User struct {
	ID int `json:"id"`
	UserInput
}

var (
	mu     sync.Mutex
	users  = map[int]User{}
	nextID = 1
)

func Count() int {
	mu.Lock()
	defer mu.Unlock()
	return len(users)
}

func Create(in UserInput) User {
	mu.Lock()
	defer mu.Unlock()
	u := User{ID: nextID, UserInput: in}
	users[u.ID] = u
	nextID++
	return u
}

func Get(id int) (User, bool) {
	mu.Lock()
	defer mu.Unlock()
	u, ok := users[id]
	return u, ok
}

func List(limit int) []User {
	mu.Lock()
	defer mu.Unlock()
	out := []User{}
	for _, id := range slices.Sorted(maps.Keys(users)) {
		if len(out) == limit {
			break
		}
		out = append(out, users[id])
	}
	return out
}

func Delete(id int) bool {
	mu.Lock()
	defer mu.Unlock()
	_, ok := users[id]
	delete(users, id)
	return ok
}

package app

import (
	"context"
	"net/http"
	"strconv"

	"github.com/swiftly-headquarters/minato/example/store"
	"github.com/swiftly-headquarters/minato/server"
)

func registerUsers(api *server.API) {
	tag := func(o *server.Operation) { o.Tags = []string{"users"} }
	server.Get(api, "/users", listUsers, tag, summary("List users"))
	server.Post(api, "/users", createUser, tag, func(o *server.Operation) {
		o.Summary = "Create a user"
		o.Description = `The role defaults to "member".`
		o.DefaultStatus = http.StatusCreated
	})
	server.Get(api, "/users/{id}", getUser, tag, summary("Fetch a user by ID"), errs(404))
	server.Delete(api, "/users/{id}", deleteUser, tag, summary("Remove a user"), errs(404))
	server.Patch(api, "/users/{id}", patchUser, tag, summary("Change some fields of a user"), errs(404))
}

type PatchBody struct {
	Name server.Optional[string] `json:"name" minLength:"1" maxLength:"64"`
	Role server.Optional[string] `json:"role" enum:"admin,member"`
}

func (b *PatchBody) Validate(ctx context.Context) []error {
	if !b.Name.Set && !b.Role.Set {
		return []error{server.FieldError("body", "send at least one of name or role")}
	}
	return nil
}

type PatchInput struct {
	ID   int `path:"id" minimum:"1"`
	Body PatchBody
}

func patchUser(ctx context.Context, in *PatchInput) (*GetOutput, error) {
	u, ok := store.Update(in.ID, func(u *store.User) {
		if in.Body.Name.Set {
			u.Name = in.Body.Name.Value
		}
		if in.Body.Role.Set {
			u.Role = in.Body.Role.Value
		}
	})
	if !ok {
		return nil, server.Error404("user not found")
	}
	return &GetOutput{Body: u}, nil
}

type ListInput struct {
	server.Page
}

type ListOutput struct {
	Body server.PageBody[store.User]
}

func listUsers(ctx context.Context, in *ListInput) (*ListOutput, error) {
	after := 0
	if in.Cursor != "" {
		if err := server.DecodeCursor(in.Cursor, &after); err != nil {
			return nil, server.InvalidCursor(err)
		}
	}
	rows := store.List(after, in.Limit+1)
	return &ListOutput{Body: server.Paginate(rows, in.Limit, func(u store.User) any { return u.ID })}, nil
}

type CreateInput struct {
	Body store.UserInput
}

type CreateOutput struct {
	Location string `header:"Location"`
	Body     store.User
}

func createUser(ctx context.Context, in *CreateInput) (*CreateOutput, error) {
	u := store.Create(in.Body)
	return &CreateOutput{Location: "/users/" + strconv.Itoa(u.ID), Body: u}, nil
}

type UserIDInput struct {
	ID int `path:"id" minimum:"1"`
}

type GetOutput struct {
	Body store.User
}

func getUser(ctx context.Context, in *UserIDInput) (*GetOutput, error) {
	u, ok := store.Get(in.ID)
	if !ok {
		return nil, server.Error404("user not found")
	}
	return &GetOutput{Body: u}, nil
}

func deleteUser(ctx context.Context, in *UserIDInput) (*struct{}, error) {
	if !store.Delete(in.ID) {
		return nil, server.Error404("user not found")
	}
	return nil, nil
}

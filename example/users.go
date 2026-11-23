package main

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
}

type ListInput struct {
	Limit int `query:"limit" default:"20" minimum:"1" maximum:"100"`
}

type ListOutput struct {
	Body []store.User
}

func listUsers(ctx context.Context, in *ListInput) (*ListOutput, error) {
	return &ListOutput{Body: store.List(in.Limit)}, nil
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

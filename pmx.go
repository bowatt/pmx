package pmx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidRef = errors.New("invalid ref")
	ErrNoRows     = pgx.ErrNoRows
	ErrNoTableTag = errors.New("no table tag")
)

type Executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func Insert(ctx context.Context, e Executor, entity any) (pgconn.CommandTag, error) {
	t := reflect.TypeOf(entity)
	v := reflect.ValueOf(entity)
	uuidT := reflect.TypeOf(uuid.Nil)

	if t.Kind() != reflect.Ptr {
		return pgconn.CommandTag{}, ErrInvalidRef
	}

	t = t.Elem()
	v = v.Elem()

	if t.Kind() != reflect.Struct {
		return pgconn.CommandTag{}, ErrInvalidRef
	}

	tableTag, ok := t.Field(0).Tag.Lookup("table")
	if !ok {
		return pgconn.CommandTag{}, ErrNoTableTag
	}

	buf := bytes.NewBufferString(fmt.Sprintf(
		"insert into %s ",
		tableTag,
	))

	columns := []string{}
	values := []string{}
	args := []any{}

	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag
		column := tag.Get("db")
		if len(column) == 0 {
			continue
		}
		if !v.Field(i).CanInterface() {
			continue
		}
		columns = append(columns, column)
		if tag.Get("default") == "true" {
			values = append(values, "default")
			continue
		}

		fv := v.Field(i)
		if fv.Kind() == reflect.Ptr && fv.IsNil() {
			args = append(args, nil)
			values = append(values, fmt.Sprintf("$%d", len(args)))
			continue
		}

		switch {
		case fv.Type() == uuidT || fv.Type().ConvertibleTo(uuidT):
			u := fv.Convert(uuidT).Interface().(uuid.UUID)
			if u == uuid.Nil {
				args = append(args, nil)
				values = append(values, fmt.Sprintf("$%d", len(args)))
				continue
			}
		case fv.Kind() == reflect.Ptr &&
			(fv.Type().Elem() == uuidT || fv.Type().Elem().ConvertibleTo(uuidT)):
			u := fv.Elem().Convert(uuidT).Interface().(uuid.UUID)
			if u == uuid.Nil {
				args = append(args, nil)
				values = append(values, fmt.Sprintf("$%d", len(args)))
				continue
			}
		}

		args = append(args, fv.Interface())
		values = append(values, fmt.Sprintf("$%d", len(args)))
	}

	buf.WriteString(fmt.Sprintf(
		"(%s) values (%s)",
		strings.Join(columns, ", "),
		strings.Join(values, ", "),
	))

	if slices.Contains(values, "default") {
		buf.WriteString(" returning *")
		rows, err := e.Query(ctx, buf.String(), args...)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		defer rows.Close()
		err = scan(rows, entity)
		if err != nil {
			return pgconn.CommandTag{}, err
		}
		rows.Close()
		return rows.CommandTag(), nil
	}

	return e.Exec(ctx, buf.String(), args...)
}

func Select(ctx context.Context, e Executor, dest any, sql string, args ...any) error {
	rows, err := e.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	return scan(rows, dest)
}

func UniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	ok := errors.As(err, &pgErr)
	return ok && pgErr.Code == pgerrcode.UniqueViolation
}

func scan(rows pgx.Rows, dest any) error {
	t := reflect.TypeOf(dest)
	if t.Kind() != reflect.Ptr {
		return ErrInvalidRef
	}

	t = t.Elem()
	v := reflect.ValueOf(dest)

	switch t.Kind() {
	case reflect.Slice:
		return scanSlice(rows, t, v)
	case reflect.Struct:
		return scanStruct(rows, t, v)
	default:
		return ErrInvalidRef
	}
}

func scanSlice(rows pgx.Rows, t reflect.Type, v reflect.Value) error {
	t = t.Elem()
	if t.Kind() != reflect.Ptr {
		return ErrInvalidRef
	}

	t = t.Elem()
	if t.Kind() != reflect.Struct {
		return ErrInvalidRef
	}

	for rows.Next() {
		ptr, err := scanFields(rows, t)
		if err != nil {
			return err
		}
		sv := v.Elem()
		sv.Set(reflect.Append(sv, ptr))
	}

	err := rows.Err()
	if err != nil {
		return err
	}

	return nil
}

func scanStruct(rows pgx.Rows, t reflect.Type, v reflect.Value) error {
	if !rows.Next() {
		err := rows.Err()
		if err != nil {
			return err
		}

		return pgx.ErrNoRows
	}

	ptr, err := scanFields(rows, t)
	if err != nil {
		return err
	}

	v.Elem().Set(ptr.Elem())
	return nil
}

func scanFields(rows pgx.Rows, t reflect.Type) (reflect.Value, error) {
	fields := []any{}
	ptr := reflect.New(t)
	v := ptr.Elem()

	for _, fd := range rows.FieldDescriptions() {
		var field any
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).Tag.Get("db") != fd.Name {
				continue
			}
			field = v.Field(i).Addr().Interface()
		}
		fields = append(fields, field)
	}

	for i := range fields {
		if len(rows.RawValues()[i]) == 0 {
			fields[i] = new(any)
		}
	}

	err := rows.Scan(fields...)
	if err != nil {
		return reflect.ValueOf(nil), err
	}

	return ptr, nil
}

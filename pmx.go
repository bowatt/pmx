package pmx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrInvalidRef       = errors.New("invalid ref")
	ErrNoRows           = pgx.ErrNoRows
	ErrNoTableTag       = errors.New("no table tag")
	ErrNothingToInsert  = errors.New("nothing to insert")
	ErrInvalidBatchSize = errors.New("invalid batch size")
)

type Executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func InsertMany(ctx context.Context, e Executor, batchSize int, entity any) ([]pgconn.CommandTag, error) {
	if batchSize < 1 || batchSize >= math.MaxUint16 {
		return nil, ErrInvalidBatchSize
	}
	t := reflect.TypeOf(entity)
	v := reflect.ValueOf(entity)
	uuidT := reflect.TypeOf(uuid.Nil)

	if t == nil || t.Kind() != reflect.Ptr || v.IsNil() {
		return nil, ErrInvalidRef
	}

	t = t.Elem()
	v = v.Elem()
	if t.Kind() != reflect.Slice {
		return nil, ErrInvalidRef
	}

	if v.Len() == 0 {
		return nil, ErrNothingToInsert
	}

	t = t.Elem()
	if t.Kind() != reflect.Ptr {
		return nil, ErrInvalidRef
	}

	t = t.Elem()
	if t.Kind() != reflect.Struct {
		return nil, ErrInvalidRef
	}

	tableTag, err := getTable(t)
	if err != nil {
		return nil, err
	}

	columns := []string{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag
		column := tag.Get("db")
		if len(column) == 0 {
			continue
		}
		columns = append(columns, column)
	}

	if len(columns)*batchSize > math.MaxUint16 {
		return nil, ErrInvalidBatchSize
	}

	var batches []reflect.Value
	for i := range (v.Len() + batchSize - 1) / batchSize {
		sliceStart := i * batchSize
		sliceEnd := min((i+1)*batchSize, v.Len())
		batches = append(batches, v.Slice(sliceStart, sliceEnd))
	}

	var batchResults []pgconn.CommandTag
	for _, batch := range batches {
		allValues := []string{}
		args := []any{}

		for i := range batch.Len() {
			values := []string{}
			arrVal := batch.Index(i)
			if arrVal.Kind() != reflect.Ptr {
				return nil, ErrInvalidRef
			}
			if arrVal.IsNil() {
				continue
			}
			arrVal = arrVal.Elem()

			if arrVal.Kind() != reflect.Struct {
				return nil, ErrInvalidRef
			}
			for j := 0; j < t.NumField(); j++ {
				tag := t.Field(j).Tag
				column := tag.Get("db")
				if len(column) == 0 {
					continue
				}
				fv := arrVal.Field(j)
				if !fv.CanInterface() {
					continue
				}
				if tag.Get("default") == "true" {
					values = append(values, "default")
					continue
				}

				if fv.Kind() == reflect.Ptr && fv.IsNil() {
					args = append(args, nil)
					values = append(values, fmt.Sprintf("$%d", len(args)))
					continue
				}

				switch {
				case (fv.Type() == uuidT || fv.Type().ConvertibleTo(uuidT)) && fv.CanConvert(uuidT):
					u := fv.Convert(uuidT).Interface().(uuid.UUID)
					if u == uuid.Nil {
						args = append(args, nil)
						values = append(values, fmt.Sprintf("$%d", len(args)))
						continue
					}
				case fv.Kind() == reflect.Ptr &&
					(fv.Type().Elem() == uuidT || fv.Type().Elem().ConvertibleTo(uuidT)) && fv.Elem().CanConvert(uuidT):
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
			allValues = append(allValues, "("+strings.Join(values, ", ")+")")
		}

		if len(allValues) == 0 {
			continue
		}

		buf := bytes.NewBufferString(fmt.Sprintf("insert into %s ", tableTag))
		buf.WriteString(fmt.Sprintf(
			"(%s) values %s",
			strings.Join(columns, ", "),
			strings.Join(allValues, ", "),
		))

		if strings.Contains(strings.Join(allValues, ", "), "default") {
			buf.WriteString(" returning *")
			rows, err := e.Query(ctx, buf.String(), args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			err = scan(rows, entity)
			if err != nil {
				return nil, err
			}
			rows.Close()

			batchResults = append(batchResults, rows.CommandTag())
			continue
		}

		tag, err := e.Exec(ctx, buf.String(), args...)
		if err != nil {
			return nil, err
		}

		batchResults = append(batchResults, tag)
	}

	if len(batchResults) == 0 {
		return nil, ErrNothingToInsert
	}

	return batchResults, nil
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

	tableTag, err := getTable(t)
	if err != nil {
		return pgconn.CommandTag{}, err
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
		case (fv.Type() == uuidT || fv.Type().ConvertibleTo(uuidT)) && fv.CanConvert(uuidT):
			u := fv.Convert(uuidT).Interface().(uuid.UUID)
			if u == uuid.Nil {
				args = append(args, nil)
				values = append(values, fmt.Sprintf("$%d", len(args)))
				continue
			}
		case fv.Kind() == reflect.Ptr &&
			(fv.Type().Elem() == uuidT || fv.Type().Elem().ConvertibleTo(uuidT)) && fv.Elem().CanConvert(uuidT):
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

func getTable(t reflect.Type) (string, error) {
	if t.NumField() == 0 {
		return "", ErrNoTableTag
	}

	tableTag, ok := t.Field(0).Tag.Lookup("table")
	if !ok {
		return "", ErrNoTableTag
	}

	return tableTag, nil
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
		field := findFieldByDBTag(t, v, fd.Name)
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

func findFieldByDBTag(t reflect.Type, v reflect.Value, name string) any {
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		fv := v.Field(i)

		tag := sf.Tag.Get("db")
		tagName, inline := parseDBTag(tag)

		if tagName == name {
			return fv.Addr().Interface()
		}

		if inline && sf.Anonymous && sf.Type.Kind() == reflect.Struct {
			if field := findFieldByDBTag(sf.Type, fv, name); field != nil {
				return field
			}
		}
	}

	return nil
}

func parseDBTag(tag string) (name string, inline bool) {
	if tag == "" {
		return "", false
	}

	parts := strings.Split(tag, ",")
	name = parts[0]

	for _, p := range parts[1:] {
		if p == "inline" {
			inline = true
			break
		}
	}

	return name, inline
}

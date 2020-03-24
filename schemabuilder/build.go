package schemabuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/unrotten/graphql/system"
	"go/ast"
	"reflect"
	"strings"
	"time"
)

// schemaBuilder is a struct for holding all the graph information for types as
// we build out graphql types for our graphql schema.  Resolved graphQL "types"
// are stored in the type map which we can use to see sections of the graph.
type schemaBuilder struct {
	types           map[reflect.Type]system.Type
	inputObjResolve map[string]func(arg interface{}) (interface{}, error)
	objects         map[reflect.Type]*Object
	enums           map[reflect.Type]*Enum
	inputObjects    map[reflect.Type]*InputObject
	interfaces      map[reflect.Type]*Interface
	scalars         map[reflect.Type]*Scalar
	unions          map[reflect.Type]*Union
}

var Serialize = func(value interface{}) (interface{}, error) {
	switch v := value.(type) {
	case string, float64, int64, bool, int, int8, int16, int32, uint, uint8, uint16, uint32, uint64, float32, time.Time:
		return v, nil
	case *string, *float64, *int64, *bool, *int, *int8, *int16, *int32, *uint, *uint8, *uint16, *uint32, *uint64, *float32, *time.Time:
		return v, nil
	case []byte:
		return string(v), nil
	case *[]byte:
		return string(*v), nil
	default:
		marshal, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return string(marshal), nil
	}

}

// getType is the "core" function of the GraphQL schema builder.  It takes in a reflect type and builds the appropriate graphQL "type".
// This includes going through struct fields and attached object methods to generate the entire graphql graph of possible queries.
// This function will be called recursively for types as we go through the graph.
func (sb *schemaBuilder) getType(nodeType reflect.Type) (system.Type, error) {
	if typ, ok := sb.types[nodeType]; ok {
		return typ, nil
	}

	// Support scalars and optional scalars. Scalars have precedence over structs to have eg. time.Time function as a scalar.
	// Enum
	if enum := sb.getEnum(nodeType); enum != nil {
		sb.types[nodeType] = &system.NonNull{Type: enum}
		sb.types[reflect.PtrTo(nodeType)] = enum
		return sb.types[nodeType], nil
	}
	if nodeType.Kind() == reflect.Ptr {
		if enum := sb.getEnum(nodeType.Elem()); enum != nil {
			sb.types[nodeType] = enum
			sb.types[nodeType.Elem()] = &system.NonNull{Type: enum}
			return sb.types[nodeType], nil
		}
	}
	// Scalar
	if scalar := sb.getScalar(nodeType); scalar != nil {
		sb.types[nodeType] = &system.NonNull{Type: scalar}
		sb.types[reflect.PtrTo(nodeType)] = scalar
		return sb.types[nodeType], nil
	}
	if nodeType.Kind() == reflect.Ptr {
		if scalar := sb.getScalar(nodeType.Elem()); scalar != nil {
			sb.types[nodeType] = scalar
			sb.types[nodeType.Elem()] = &system.NonNull{Type: scalar}
			return sb.types[nodeType], nil // XXX: prefix typ with "*"
		}
	}
	// Interface
	if nodeType.Kind() == reflect.Interface {
		if inter, err := sb.getInterface(nodeType); inter != nil {
			return sb.types[nodeType], nil
		} else if err != nil {
			return nil, err
		}
	}
	if nodeType.Kind() == reflect.Ptr && nodeType.Elem().Kind() == reflect.Interface {
		if inter, err := sb.getInterface(nodeType.Elem()); inter != nil {
			return sb.types[nodeType], nil
		} else if err != nil {
			return nil, err
		}
	}

	// Union / Input Object / Object
	if nodeType.Kind() == reflect.Struct {
		if err := sb.buildStruct(nodeType); err != nil {
			return nil, err
		}
		return sb.types[nodeType], nil
	}
	if nodeType.Kind() == reflect.Ptr && nodeType.Elem().Kind() == reflect.Struct {
		if err := sb.buildStruct(nodeType.Elem()); err != nil {
			return nil, err
		}
		return sb.types[nodeType], nil
	}

	if nodeType.Kind() == reflect.Slice {
		elementType, err := sb.getType(nodeType.Elem())
		if err != nil {
			return nil, err
		}
		sb.types[nodeType] = &system.List{Type: elementType}
		sb.types[reflect.PtrTo(nodeType)] = &system.List{Type: elementType}
		return sb.types[nodeType], nil
	}
	return nil, fmt.Errorf("bad type %s: should be a scalar, slice, or struct type", nodeType)
}

// getEnum gets the Enum type information for the passed in reflect.Operation by looking it up in our enum mappings.
func (sb *schemaBuilder) getEnum(typ reflect.Type) *system.Enum {
	if enum, ok := sb.enums[typ]; ok {
		var values []string
		for mapping := range enum.Map {
			values = append(values, mapping)
		}
		return &system.Enum{
			Name:       enum.Name,
			Values:     values,
			ValuesDesc: enum.DescMap,
			ReverseMap: enum.Map,
			Map:        enum.ReverseMap,
			Desc:       enum.Desc,
		}
	}
	return nil
}

// getScalar grabs the appropriate scalar graphql field type name for the passed
// in variable reflect type.
func (sb *schemaBuilder) getScalar(typ reflect.Type) *system.Scalar {
	if scalar, ok := sb.scalars[typ]; ok {
		return &system.Scalar{
			Name:         scalar.Name,
			Desc:         scalar.Desc,
			Serialize:    scalar.Serialize,
			ParseValue:   scalar.ParseValue,
			ParseLiteral: scalar.ParseLiteral,
		}
	}
	return nil
}

func (sb *schemaBuilder) getInterface(typ reflect.Type) (*system.Interface, error) {
	if inter, ok := sb.interfaces[typ]; ok {
		if len(inter.FieldResolve) == 0 {
			return nil, fmt.Errorf("interface %s should had at least one field", inter.Name)
		}
		iface := &system.Interface{
			Name:       inter.Name,
			Desc:       inter.Desc,
			Interfaces: map[string]*system.Interface{},
		}
		sb.types[typ] = iface
		sb.types[reflect.PtrTo(typ)] = iface
		fields := make(map[string]*system.Field)
		for name, resolve := range inter.FieldResolve {
			if mname, ok := resolve.fn.(string); ok {
				method, ok := typ.MethodByName(mname)
				if !ok {
					return nil, fmt.Errorf("%s should be method of %s", mname, typ.String())
				}
				fctx := &funcContext{funcType: method.Type}
				// Parse return values. The first return value must be the actual value, and the second value can optionally be an error.
				err := fctx.parseReturnSignature(resolve)
				if err != nil {
					return nil, err
				}

				retType, err := fctx.getReturnType(sb, resolve)
				if err != nil {
					return nil, err
				}
				fields[name] = &system.Field{
					Name: name,
					Type: retType,
					Desc: resolve.desc,
				}
				continue
			}
			f, err := sb.getField(resolve, typ)
			if err != nil {
				return nil, err
			}
			f.Name = name
			fields[name] = f
		}
		var function system.TypeResolve
		if inter.Fn != nil {
			var err error
			function, err = sb.getTypeFunction(inter.Fn, typ)
			if err != nil {
				return nil, err
			}
		}

		possibleTypes := make(map[string]*system.Object)
		for name, object := range inter.PossibleTypes {
			t, err := sb.getType(reflect.TypeOf(object.Type))
			if err != nil {
				return nil, err
			}
			possibleTypes[name] = t.(*system.NonNull).Type.(*system.Object)
		}
		iface.Fields = fields
		iface.PossibleTypes = possibleTypes
		iface.TypeResolve = function
		for _, innerIface := range inter.Interface {
			innerIfaceTyp, err := sb.getType(reflect.TypeOf(innerIface.Type))
			if err != nil {
				return nil, err
			}
			iface.Interfaces[innerIface.Name] = innerIfaceTyp.(*system.Interface)
		}
		return iface, nil
	}
	return nil, nil
}

func (sb *schemaBuilder) buildStruct(typ reflect.Type) error {
	// Union
	if _, ok := sb.unions[typ]; ok {
		return sb.buildUnion(typ)
	}
	// Input Object
	if _, ok := sb.inputObjects[typ]; ok {
		return sb.builInputObject(typ)
	}
	// Object
	if obj, ok := sb.objects[typ]; ok {
		object := &system.Object{
			Name:       obj.Name,
			Desc:       obj.Desc,
			Interfaces: map[string]*system.Interface{},
			Fields:     map[string]*system.Field{},
			IsTypeOf:   reflect.New(typ).Interface(),
		}

		sb.types[reflect.PtrTo(typ)] = object
		sb.types[typ] = &system.NonNull{Type: object}
		for name, resolve := range obj.FieldResolve {
			if f, err := sb.getField(resolve, typ); err == nil && f != nil {
				f.Name = name
				if args, ok := obj.ArgDefault[name]; ok {
					for argName, defaultValue := range args {
						f.Args[argName].DefaultValue = defaultValue
					}
				}
				object.Fields[name] = f
			} else if err != nil {
				return fmt.Errorf("object %s field %s parse error:%w", typ.String(), name, err)
			}
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := field.Name
			var desc string
			var nonnull bool
			if tag := field.Tag.Get("graphql"); tag == "-" {
				continue
			} else if tag != "" {
				split := strings.Split(tag, ";")
				name = split[0]
				if len(split) > 1 {
					desc = split[1]
				}
				if len(split) > 2 {
					nonnull = split[2] == "nonnull"
				}
			}
			if _, ok := obj.FieldResolve[name]; ok {
				continue
			}

			fieldTyp, err := sb.getType(field.Type)
			if err != nil {
				return err
			}
			if _, ok := fieldTyp.(*system.InputObject); ok {
				return fmt.Errorf("object %s field %s type can not be input object", typ.String(), name)
			}
			if nonnull {
				fieldTyp = &system.NonNull{Type: fieldTyp}
			}
			object.Fields[name] = &system.Field{
				Name: name,
				Type: fieldTyp,
				Args: map[string]*system.Argument{},
				Resolve: func(ctx context.Context, source, args interface{}) (interface{}, error) {
					if source == nil {
						return nil, fmt.Errorf("source is nil")
					}
					value := reflect.ValueOf(source)
					if value.Kind() == reflect.Ptr {
						value = value.Elem()
					}
					fieldVal := GetField(value, name)
					if fieldVal == nil {
						return nil, fmt.Errorf("can not get field %s", name)
					}
					return (*fieldVal).Interface(), nil
				},
				Desc: desc,
			}
			object.Fields[name].Type = fieldTyp
		}
		for _, iface := range obj.Interface {
			ifaceTyp, err := sb.getType(reflect.TypeOf(iface.Type))
			if err != nil {
				return err
			}
			for f := range ifaceTyp.(*system.Interface).Fields {
				if _, ok := object.Fields[f]; !ok {
					return fmt.Errorf("%s must impl interface field %s", object.Name, f)
				}
			}
			object.Interfaces[iface.Name] = ifaceTyp.(*system.Interface)
		}
		return nil
	}
	return nil
}

func (sb *schemaBuilder) buildUnion(typ reflect.Type) error {
	union := sb.unions[typ]
	unionTyp := &system.Union{
		Name:  union.Name,
		Desc:  union.Desc,
		Types: make(map[string]*system.Object, typ.NumField()),
	}
	sb.types[reflect.PtrTo(typ)] = unionTyp
	sb.types[typ] = &system.NonNull{Type: unionTyp}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Type.Kind() != reflect.Ptr && field.Type.Elem().Kind() != reflect.Struct {
			return fmt.Errorf("%s %s %s: union's field must be struct's prt", field.PkgPath, typ.String(), field.Name)
		}
		if _, ok := sb.objects[field.Type.Elem()]; !ok {
			return fmt.Errorf("%s %s %s: union's field type must be object", field.PkgPath, typ.String(), field.Name)
		}
		object, err := sb.getType(field.Type)
		if err != nil {
			return err
		}
		unionTyp.Types[object.(*system.Object).Name] = object.(*system.Object)
	}
	return nil
}

func (sb *schemaBuilder) builInputObject(typ reflect.Type) error {
	input := sb.inputObjects[typ]
	inputObject := &system.InputObject{
		Name:   input.Name,
		Fields: map[string]*system.InputField{},
		Desc:   input.Desc,
	}
	sb.types[reflect.PtrTo(typ)] = inputObject
	sb.types[typ] = &system.NonNull{Type: inputObject}
	resolveMap := make(map[string]func(interface{}) (interface{}, error))
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := field.Name
		if !ast.IsExported(name) {
			return fmt.Errorf("input object field must be exported")
		}
		if tag := field.Tag.Get("graphql"); tag == "-" {
			continue
		} else if tag != "" {
			split := strings.Split(tag, ";")
			name = split[0]
		}
		var defaultValue interface{}
		if f, ok := input.Fields[name]; ok {
			defaultValue = f.DefaultValue
		}
		fieldTyp, err := sb.getType(field.Type)
		if err != nil {
			return err
		}
		resolve, err := sb.getArgResolve(fieldTyp)
		if err != nil {
			return err
		}
		resolveMap[name] = resolve
		inputObject.Fields[name] = &system.InputField{
			Name:         name,
			Type:         fieldTyp,
			DefaultValue: defaultValue,
		}
		inputObject.Fields[name].Type = fieldTyp
	}
	sb.inputObjResolve[inputObject.Name] = func(args interface{}) (interface{}, error) {
		if args == nil {
			args = make(map[string]interface{})
		}
		for name, field := range inputObject.Fields {
			if args.(map[string]interface{})[name] == nil && field.DefaultValue != nil {
				args.(map[string]interface{})[name] = field.DefaultValue
			}
		}
		if args, ok := args.(map[string]interface{}); ok {
			for key, val := range args {
				if resolve, ok := resolveMap[key]; ok {
					value, err := resolve(val)
					if err != nil {
						return nil, err
					}
					args[key] = value
				} else {
					return nil, fmt.Errorf("unexpcted field %q for %q", key, input.Name)
				}
			}
			return Convert(args, typ)
		}
		return nil, fmt.Errorf("expected arg map but got %v", args)
	}
	return nil
}

func (sb *schemaBuilder) getField(fnresolve *FieldResolve, src reflect.Type) (*system.Field, error) {
	fctx := funcContext{typ: src}

	callableFunc, err := fctx.getFuncVal(fnresolve.fn)
	if err != nil {
		return nil, err
	}
	in := fctx.getFuncInputTypes()
	in = fctx.consumeContextAndSource(in)

	argResolve, args, in, err := fctx.getArgParserAndTyp(sb, in)
	if err != nil {
		return nil, err
	}
	fctx.hasArg = len(args) > 0

	// We have succeeded if no arguments remain.
	if len(in) != 0 {
		return nil, fmt.Errorf("%s arguments should be [context][, [*]%s][, args]", fctx.funcType, src)
	}

	// Parse return values. The first return value must be the actual value, and the second value can optionally be an error.
	err = fctx.parseReturnSignature(fnresolve)
	if err != nil {
		return nil, err
	}

	retType, err := fctx.getReturnType(sb, fnresolve)
	if err != nil {
		return nil, err
	}

	field := &system.Field{
		Type: retType,
		Args: args,
		Resolve: func(ctx context.Context, source, args interface{}) (interface{}, error) {
			// Set up function arguments.
			funcInputArgs, err := fctx.prepareResolveArgs(source, fctx.hasArg, args, argResolve, ctx)
			if err != nil {
				return nil, err
			}
			var funcOutputArgs []reflect.Value
			funcOutputArgs = callableFunc.Call(funcInputArgs)

			return fctx.extractResultAndErr(funcOutputArgs, retType)
		},
		Desc: fnresolve.desc,
	}
	for _, handler := range fnresolve.handleChain {
		field.HandlersChain = append(field.HandlersChain, func(ctx context.Context) error {
			return handler(ctx)
		})
	}

	return field, nil
}

func (sb *schemaBuilder) getTypeFunction(fn interface{}, source reflect.Type) (system.TypeResolve, error) {
	if fn == nil {
		return nil, nil
	}
	fctx := funcContext{}
	typ := reflect.TypeOf(fn)
	if typ.NumIn() > 2 {
		return nil, fmt.Errorf("interface field num in can not more than 2")
	}
	for i := 0; i < typ.NumIn(); i++ {
		inTyp := typ.In(i)
		switch inTyp {
		case reflect.TypeOf(context.Background()):
			fctx.hasContext = true
		case source, reflect.New(source).Type():
			fctx.hasSource = true
		default:
			return nil, fmt.Errorf("interface typeResolve func num in has error type")
		}
	}
	if typ.NumOut() != 1 {
		return nil, fmt.Errorf("interface field num out must be 1")
	}

	return func(ctx context.Context, value interface{}) *system.Object {
		var in []reflect.Value
		if fctx.hasContext {
			in = append(in, reflect.ValueOf(ctx))
		}
		if fctx.hasSource {
			in = append(in, reflect.ValueOf(value))
		}
		values := reflect.ValueOf(fn).Call(in)
		if len(values) > 0 {
			resTyp := values[0].Type()
			var res system.Type
			if values[0].Kind() == reflect.Interface && !values[0].IsNil() {
				resTyp = values[0].Elem().Type()
			}
			if resTyp.Kind() != reflect.Ptr {
				resTyp = reflect.PtrTo(resTyp)
			}
			res, err := sb.getType(resTyp)
			if err != nil {
				return nil
			}
			if obj, ok := res.(*system.Object); ok {
				return obj
			}
		}
		return nil
	}, nil
}

func (sb *schemaBuilder) getArguments(typ reflect.Type) (func(args interface{}) (interface{}, error), map[string]*system.Argument, error) {
	args := make(map[string]*system.Argument)
	if typ.Kind() != reflect.Struct {
		return nil, nil, fmt.Errorf("object args must be struct")
	}
	resolve := make(map[string]func(interface{}) (interface{}, error))
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := field.Name
		if !ast.IsExported(name) {
			return nil, nil, fmt.Errorf("arg field name must can exproted, but %s not", name)
		}
		exist, skip, nonnull, tagName, desc := parseFieldTag(field)
		if skip {
			continue
		}
		if exist {
			name = tagName
		}
		fieldTyp, err := sb.getType(field.Type)
		if err != nil {
			return nil, nil, err
		}
		if nonnull {
			fieldTyp = &system.NonNull{Type: fieldTyp}
		}
		argResolve, err := sb.getArgResolve(fieldTyp)
		if err != nil {
			return nil, nil, err
		}
		resolve[name] = argResolve
		args[name] = &system.Argument{
			Name: name,
			Type: fieldTyp,
			Desc: desc,
		}
	}
	return func(arg interface{}) (interface{}, error) {
		if arg == nil {
			arg = make(map[string]interface{})
		}
		for name, argument := range args {
			if arg.(map[string]interface{})[name] == nil && argument.DefaultValue != nil {
				arg.(map[string]interface{})[name] = argument.DefaultValue
			}
		}
		if arg, ok := arg.(map[string]interface{}); ok {
			for key, val := range arg {
				value, err := resolve[key](val)
				if err != nil {
					return nil, err
				}
				arg[key] = value
			}
			return Convert(arg, typ)
		}
		return nil, fmt.Errorf("expected arg map but got %v", arg)
	}, args, nil
}

func (sb *schemaBuilder) getArgResolve(typ system.Type) (func(interface{}) (interface{}, error), error) {
	switch typ := typ.(type) {
	case *system.Scalar:
		return func(value interface{}) (interface{}, error) {
			if value == nil {
				return nil, nil
			}
			return typ.ParseValue(value)
		}, nil
	case *system.Enum:
		return func(value interface{}) (interface{}, error) {
			if value == nil {
				return nil, nil
			}
			if _, ok := value.(string); !ok {
				return nil, fmt.Errorf("enum value must be string")
			}
			return typ.ReverseMap[value.(string)], nil
		}, nil
	case *system.InputObject:
		return func(value interface{}) (interface{}, error) {
			if value == nil {
				return nil, nil
			}
			if f, ok := sb.inputObjResolve[typ.Name]; ok {
				return f(value)
			}
			return nil, nil
		}, nil
	case *system.NonNull:
		return func(value interface{}) (interface{}, error) {
			if value == nil {
				return nil, fmt.Errorf("cannot be null for nonnullable input arg: %s", typ.String())
			}
			resolve, err := sb.getArgResolve(typ.Type)
			if err != nil {
				return nil, err
			}
			return resolve(value)
		}, nil
	case *system.List:
		resolve, err := sb.getArgResolve(typ.Type)
		if err != nil {
			return nil, err
		}
		return func(value interface{}) (interface{}, error) {
			if value == nil {
				return nil, nil
			}
			v := reflect.ValueOf(value)
			var val []interface{}
			if v.Kind() != reflect.Slice {
				val = append(val, value)
			} else {
				for i := 0; i < v.Len(); i++ {
					val = append(val, v.Index(i).Interface())
				}
			}
			var res []interface{}
			for _, ival := range val {
				ival, err := resolve(ival)
				if err != nil {
					return nil, err
				}
				res = append(res, ival)
			}
			return res, nil
		}, nil
	default:
		return nil, fmt.Errorf("object field type can not be interface,union and object")
	}
}

type funcContext struct {
	hasContext      bool
	hasSource       bool
	hasArg          bool
	hasRet          bool
	hasErr          bool
	sourcePtr       bool
	sourceInterface bool
	argTyp          reflect.Type
	funcType        reflect.Type
	isPtrFunc       bool
	typ             reflect.Type

	returnsFunc    bool
	wrapperFuncTyp reflect.Type
}

func (f *funcContext) getFuncVal(fn interface{}) (reflect.Value, error) {
	fun := reflect.ValueOf(fn)
	if fun.Kind() != reflect.Func {
		return fun, fmt.Errorf("fun must be func, not %s", fun)
	}
	f.funcType = fun.Type()
	return fun, nil
}

// getFuncInputTypes returns the input arguments for the function we're representing.
func (funcCtx *funcContext) getFuncInputTypes() []reflect.Type {
	in := make([]reflect.Type, 0, funcCtx.funcType.NumIn())
	for i := 0; i < funcCtx.funcType.NumIn(); i++ {
		in = append(in, funcCtx.funcType.In(i))
	}
	return in
}

// consumeContextAndSource reads in the input parameters for the provided function and determines whether the function has a Context input parameter
// and/or whether it includes the "source" input parameter ("source" will be the object type that this function is connected to).  If we find either of these
// fields we will pop that field from the input parameters we return (since we've already "dealt" with those fields).
func (funcCtx *funcContext) consumeContextAndSource(in []reflect.Type) []reflect.Type {
	ptr := reflect.PtrTo(funcCtx.typ)

	if len(in) > 0 && in[0] == reflect.TypeOf(context.Background()) {
		funcCtx.hasContext = true
		in = in[1:]
	}

	if len(in) > 0 && (in[0] == funcCtx.typ || in[0] == ptr) {
		funcCtx.hasSource = true
		funcCtx.isPtrFunc = in[0] == ptr
		if in[0].Kind() == reflect.Interface {
			funcCtx.sourceInterface = true
		}
		in = in[1:]
	}

	return in
}

// getArgParserAndTyp reads a list of input parameters, and, if we have a set of custom parameters for the field func (at this point any input type other
// than the selectionSet is considered the args input), we will return the argParser for that type and pop that field from the returned input parameters.
func (funcCtx *funcContext) getArgParserAndTyp(sb *schemaBuilder, in []reflect.Type) (func(interface{}) (interface{}, error),
	map[string]*system.Argument, []reflect.Type, error) {
	args := make(map[string]*system.Argument)
	var resolve func(interface{}) (interface{}, error)
	if len(in) > 0 {
		var err error
		inTyp := in[0]
		resolve, args, err = sb.getArguments(inTyp)
		if err != nil {
			return nil, nil, nil, err
		}
		in = in[1:]
	}
	return resolve, args, in, nil
}

// parseReturnSignature reads and validates the return signature of the function to determine whether it has a return type and/or an error response.
func (funcCtx *funcContext) parseReturnSignature(r *FieldResolve) (err error) {
	out := make([]reflect.Type, 0, funcCtx.funcType.NumOut())
	for i := 0; i < funcCtx.funcType.NumOut(); i++ {
		out = append(out, funcCtx.funcType.Out(i))
	}

	if len(out) > 0 && out[0] != errType {
		funcCtx.hasRet = true

		if out[0].Kind() == reflect.Func {
			funcCtx.returnsFunc = true
		}

		out = out[1:]
	}

	if len(out) > 0 && out[0] == errType {
		funcCtx.hasErr = true
		out = out[1:]
	}

	if len(out) != 0 {
		err = fmt.Errorf("%s return values should [result][, error]", funcCtx.funcType)
		return
	}

	if !funcCtx.hasRet && r.MarkedNonNullable {
		err = fmt.Errorf("%s is marked non-nullable, but has no return value", funcCtx.funcType)
		return
	}

	return
}

// getReturnType returns a GraphQL node type for the return type of the function.  So an object "User" that has a linked function which returns a
// list of "Hats" will resolve the GraphQL type of a "Hat" at this point.
func (funcCtx *funcContext) getReturnType(sb *schemaBuilder, m *FieldResolve) (system.Type, error) {
	var retType system.Type
	if funcCtx.returnsFunc {
		function := funcCtx.funcType.Out(0)

		if function.NumIn() > 0 {
			return nil, fmt.Errorf("%s should have zero arguments", function)
		}

		funcCtx.funcType = function
		if funcCtx.funcType.Out(0) == errType {
			if funcCtx.hasErr {
				return nil, fmt.Errorf("%s should only return one error", function)
			}
			funcCtx.hasErr = true
			funcCtx.hasRet = false
		}
	}
	if funcCtx.hasRet {
		var err error

		retType, err = sb.getType(funcCtx.funcType.Out(0))
		if err != nil {
			return nil, err
		}

		if m.MarkedNonNullable {
			if _, ok := retType.(*system.NonNull); !ok {
				retType = &system.NonNull{Type: retType}
			}
		}
	} else {
		var err error
		retType, err = sb.getType(reflect.TypeOf(true))
		if err != nil {
			return nil, err
		}
	}
	return retType, nil
}

// prepareResolveArgs converts the provided source, args and context into the required list of reflect.Value types that the function needs to be called.
func (funcCtx *funcContext) prepareResolveArgs(source interface{}, hasArgs bool, args interface{},
	argResolve func(interface{}) (interface{}, error), ctx context.Context) ([]reflect.Value, error) {
	in := make([]reflect.Value, 0, funcCtx.funcType.NumIn())
	if funcCtx.hasContext {
		in = append(in, reflect.ValueOf(ctx))
	}

	// Set up source.
	if funcCtx.hasSource {
		sourceValue := reflect.ValueOf(source)
		sourceTyp := sourceValue.Type()
		ptrSource := sourceValue.Kind() == reflect.Ptr
		switch {
		case funcCtx.sourceInterface &&
			((ptrSource && (sourceTyp.Implements(funcCtx.typ) || sourceTyp.Elem().Implements(funcCtx.typ))) ||
				(!ptrSource && (sourceTyp.Implements(funcCtx.typ) || reflect.PtrTo(sourceTyp).Implements(funcCtx.typ)))):
			in = append(in, sourceValue.Convert(funcCtx.typ))
		case ptrSource && !funcCtx.isPtrFunc:
			in = append(in, sourceValue.Elem())
		case !ptrSource && funcCtx.isPtrFunc:
			copyPtr := reflect.New(funcCtx.typ)
			copyPtr.Elem().Set(sourceValue)
			in = append(in, copyPtr)
		default:
			in = append(in, sourceValue)
		}
	}

	// Set up other arguments.
	if hasArgs {
		args, err := argResolve(args)
		if err != nil {
			return nil, err
		}
		in = append(in, reflect.ValueOf(args))
	}

	return in, nil
}

// extractResultAndErr converts the response from calling the function into the expected type for the response object (as opposed to a reflect.Value).
// It also handles reading whether the function ended with errors.
func (funcCtx *funcContext) extractResultAndErr(out []reflect.Value, retType system.Type) (interface{}, error) {
	var result interface{}
	if funcCtx.hasRet {
		result = out[0].Interface()
		if out[0].Kind() == reflect.Func {
			call := out[0].Call(nil)
			result = call[0].Interface()
		}
		out = out[1:]
	} else {
		result = true
	}
	if funcCtx.hasErr {
		if err := out[0]; !err.IsNil() {
			if err.Kind() == reflect.Func {
				call := out[0].Call(nil)
				err = call[0]
			}
			return nil, err.Interface().(error)
		}
	}

	//if _, ok := retType.(*builder.NonNull); ok {
	//	resultValue := reflect.ValueOf(result)
	//	if k := resultValue.Kind(); (k == reflect.Ptr || k == reflect.Slice || k == reflect.Interface || k == reflect.Map) && resultValue.IsNil() {
	//		return nil, fmt.Errorf("%s is marked non-nullable but returned a null value", funcCtx.funcType)
	//	}
	//}

	return result, nil
}

var scalars = map[string]*Scalar{
	"Boolean":   Boolean,
	"Int":       Int,
	"Int8":      Int8,
	"Int16":     Int16,
	"Int32":     Int32,
	"Int64":     Int64,
	"Uint":      Uint,
	"Uint8":     Uint8,
	"Uint16":    Uint16,
	"Uint32":    Uint32,
	"Uint64":    Uint64,
	"Float":     Float,
	"Float64":   Float64,
	"String":    String,
	"ID":        ID,
	"Map":       MMap,
	"Time":      Time,
	"Bytes":     Bytes,
	"AnyScalar": AnyScalar,
}

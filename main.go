package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/fullstorydev/grpcurl"
	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto" //lint:ignore SA1019 same as above
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/grpcreflect"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/descriptorpb"
)

// customEventHandler extends the DefaultEventHandler to capture headers and trailers
type customEventHandler struct {
	*grpcurl.DefaultEventHandler
	headers  metadata.MD
	trailers metadata.MD
}

// OnReceiveTrailers captures the incoming trailer metadata
func (h *customEventHandler) OnReceiveTrailers(status *status.Status, md metadata.MD) {
	if h.trailers == nil {
		h.trailers = md
	} else {
		for k, v := range md {
			h.trailers[k] = append(h.trailers[k], v...)
		}
	}
	h.DefaultEventHandler.OnReceiveTrailers(status, md)
}

// NewGrpcReflectionServer creates a new GrpcReflectionServer for the given target address.
func NewGrpcReflectionServer(host string) *GrpcReflectionServer {
	srv := server.NewMCPServer(
		"grpcReflectionServer",
		"1.0.0",
		server.WithLogging(),
	)

	grs := &GrpcReflectionServer{
		srv:     srv,
		host:    host,
		headers: make(map[string]string),
	}

	grs.registerTools()
	return grs
}

// Serve starts the MCP server over standard I/O.
func (g *GrpcReflectionServer) Serve() error {
	return server.ServeStdio(g.srv)
}

// registerTools registers the grpcurl-based tools available via the MCP server.
func (g *GrpcReflectionServer) registerTools() {
	// Tool: set-headers
	setHeadersTool := mcp.NewTool(
		"set-headers",
		mcp.WithDescription(`Set global headers to be used with all future gRPC requests.
Parameters:
 - "headers": JSON object with header key-value pairs, e.g. {"Authorization": "Bearer <token>"}.
 - "clear": (Optional) Boolean to clear all existing headers before setting new ones.`),
		mcp.WithString("headers", mcp.Description("JSON object with header key-value pairs"), mcp.Required()),
		mcp.WithBoolean("clear", mcp.Description("Clear existing headers before setting new ones")),
	)
	g.srv.AddTool(setHeadersTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments
		headersJSON, _ := args["headers"].(string)
		clear, _ := args["clear"].(bool)

		// Parse headers
		newHeaders := make(map[string]string)
		if err := json.Unmarshal([]byte(headersJSON), &newHeaders); err != nil {
			return toolError("Failed to parse headers JSON: " + err.Error()), nil
		}

		// Clear existing headers if requested
		if clear {
			g.headers = make(map[string]string)
		}

		// Update headers
		for k, v := range newHeaders {
			g.headers[k] = v
		}

		// Format headers for display
		headersMap := make(map[string]interface{})
		for k, v := range g.headers {
			headersMap[k] = v
		}

		jsonResponse, err := json.MarshalIndent(headersMap, "", "  ")
		if err != nil {
			return toolError("Failed to marshal headers: " + err.Error()), nil
		}

		return toolSuccess(fmt.Sprintf("Headers updated successfully:\n%s", string(jsonResponse))), nil
	})

	// Tool 1: invoke
	invokeTool := mcp.NewTool(
		"invoke",
		mcp.WithDescription(`Invokes a gRPC method using reflection.
Parameters:
 - "method": Fully-qualified method name (e.g., package.Service/Method).
 - "request": JSON payload for the request.
 - "headers": (Optional) JSON object for custom gRPC headers that will be merged with global headers.`),
		mcp.WithString("method", mcp.Description("Fully-qualified method name (e.g., package.Service/Method)"), mcp.Required()),
		mcp.WithString("request", mcp.Description("JSON request payload"), mcp.Required()),
		mcp.WithString("headers", mcp.Description("Optional JSON object for request-specific headers")),
	)
	g.srv.AddTool(invokeTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := request.Params.Arguments
		method, _ := args["method"].(string)
		reqPayload, _ := args["request"].(string)
		requestHeadersJSON, _ := args["headers"].(string)

		// Parse request-specific headers if provided and merge with global headers
		headers := []string{}

		// Start with global headers
		for k, v := range g.headers {
			headers = append(headers, fmt.Sprintf("%s: %s", k, v))
		}

		// Add request-specific headers if provided
		if requestHeadersJSON != "" {
			requestHeaders := make(map[string]string)
			if err := json.Unmarshal([]byte(requestHeadersJSON), &requestHeaders); err != nil {
				return toolError("Failed to parse headers JSON: " + err.Error()), nil
			}
			for k, v := range requestHeaders {
				headers = append(headers, fmt.Sprintf("%s: %s", k, v))
			}
		}

		// Create a gRPC client connection.
		network := "tcp"
		target := g.host
		dialTime := 10 * time.Second

		dialOptions := []grpc.DialOption{
			grpc.WithBlock(),
			grpc.WithTimeout(dialTime),
			grpc.WithInsecure(), // adjust based on security requirements.
		}

		cc, err := grpcurl.BlockingDial(ctx, network, target, nil, dialOptions...)
		if err != nil {
			return toolError("Failed to create gRPC connection: " + err.Error()), nil
		}
		defer cc.Close()

		// Create a reflection client and descriptor source.
		refClient := grpcreflect.NewClient(ctx, grpc_reflection_v1alpha.NewServerReflectionClient(cc))
		defer refClient.Reset()
		descSource := grpcurl.DescriptorSourceFromServer(ctx, refClient)

		// Create an in-memory buffer to capture output.
		var outputBuffer bytes.Buffer

		// Create a formatter (we don't need the parser in the new API).
		_, formatter, err := grpcurl.RequestParserAndFormatter(grpcurl.FormatJSON, descSource, &outputBuffer, grpcurl.FormatOptions{})
		if err != nil {
			return toolError("Failed to create formatter: " + err.Error()), nil
		}

		// Create a custom event handler with header capture capability
		handler := &customEventHandler{
			DefaultEventHandler: &grpcurl.DefaultEventHandler{
				Out:            &outputBuffer,
				Formatter:      formatter,
				VerbosityLevel: 0,
				NumResponses:   0,
				Status:         nil,
			},
		}

		// Create a request supplier that supplies a single JSON message.
		reqSupplier := &singleMessageSupplier{
			data: []byte(reqPayload),
		}

		// Invoke the gRPC method using the new API signature.
		err = grpcurl.InvokeRPC(ctx, descSource, cc, method, headers, handler, reqSupplier.Supply)
		if err != nil {
			return toolError("Failed to invoke RPC: " + err.Error()), nil
		}

		// Check if there was an error status from the RPC
		if handler.Status != nil && handler.Status.Err() != nil {
			return toolError(fmt.Sprintf("RPC failed: %v", handler.Status.Err())), nil
		}

		// Convert metadata.MD to map for JSON marshaling
		headersMap := metadataToMap(handler.headers)
		trailersMap := metadataToMap(handler.trailers)

		// Create a structured response with headers and trailers
		response := map[string]interface{}{
			"body":     outputBuffer.String(),
			"headers":  headersMap,
			"trailers": trailersMap,
			"metadata": map[string]interface{}{
				"status_code": handler.Status.Code().String(),
			},
		}

		// Convert the response to JSON.
		jsonResponse, err := json.MarshalIndent(response, "", "  ")
		if err != nil {
			return toolError("Failed to marshal response: " + err.Error()), nil
		}

		// Return the structured response.
		return toolSuccess(string(jsonResponse)), nil
	})

	// Tool 2: list
	listTool := mcp.NewTool(
		"list",
		mcp.WithDescription("Lists all available gRPC services on the target server using reflection."),
	)
	g.srv.AddTool(listTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Create a gRPC client connection
		network := "tcp"
		target := g.host
		dialTime := 10 * time.Second

		dialOptions := []grpc.DialOption{
			grpc.WithBlock(),
			grpc.WithTimeout(dialTime),
			grpc.WithInsecure(),
		}

		cc, err := grpcurl.BlockingDial(ctx, network, target, nil, dialOptions...)
		if err != nil {
			return toolError("Failed to create gRPC connection: " + err.Error()), nil
		}
		defer cc.Close()

		// Create a reflection client
		refClient := grpcreflect.NewClient(ctx, grpc_reflection_v1alpha.NewServerReflectionClient(cc))
		defer refClient.Reset()

		// List all services
		services, err := refClient.ListServices()
		if err != nil {
			return toolError("Failed to list services: " + err.Error()), nil
		}

		// Format the output similarly to grpcurl
		var output strings.Builder
		for _, svc := range services {
			if svc != "grpc.reflection.v1alpha.ServerReflection" {
				output.WriteString(svc)
				output.WriteString("\n")
			}
		}

		return toolSuccess(output.String()), nil
	})

	// Tool 3: describe
	describeTool := mcp.NewTool(
		"describe",
		mcp.WithDescription(`Describes a gRPC service or message type.
Provide the target entity using dot notation.
Examples:
 - "mypackage.MyService" to describe the service.
 - "mypackage.MyMessage.MyRpc" to describe a specific RPC method.
 - "mypackage.MyMessage" to describe a message type.
Note: Slash notation (e.g., "mypackage.MyService/MyMethod") is used for invoking RPCs, not for describing symbols.`),
		WithStringArray("entities", mcp.Description("The services or messages type to describe (use dot notation)"), mcp.Required()),
	)
	g.srv.AddTool(describeTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Create a gRPC client connection
		network := "tcp"
		target := g.host
		dialTime := 10 * time.Second

		dialOptions := []grpc.DialOption{
			grpc.WithBlock(),
			grpc.WithTimeout(dialTime),
			grpc.WithInsecure(),
		}

		cc, err := grpcurl.BlockingDial(ctx, network, target, nil, dialOptions...)
		if err != nil {
			return toolError("Failed to create gRPC connection: " + err.Error()), nil
		}
		defer cc.Close()

		// Create a reflection client and descriptor source
		refClient := grpcreflect.NewClient(ctx, grpc_reflection_v1alpha.NewServerReflectionClient(cc))
		defer refClient.Reset()
		descSource := grpcurl.DescriptorSourceFromServer(ctx, refClient)

		args := request.Params.Arguments
		entities, ok := args["entities"].(string)
		var tmp []string
		if ok {
			tmp = strings.Split(entities, ",")
		} else if entities, ok := args["entities"].([]interface{}); ok {
			for _, entity := range entities {
				entityStr, ok := entity.(string)
				if !ok {
					given, _ := json.Marshal(entity)
					return toolError(fmt.Sprintf("entities argument should be an array of strings instead of %s", given)), nil
				}
				tmp = append(tmp, entityStr)
			}
		}

		// Split the entities by comma
		if len(tmp) == 0 {
			return toolError("No entities provided"), nil
		}

		var results []string

		for _, entityStr := range tmp {
			// Remove leading dot if present
			if entityStr != "" && entityStr[0] == '.' {
				entityStr = entityStr[1:]
			}

			// Find the symbol
			dsc, err := descSource.FindSymbol(entityStr)
			if err != nil {
				return toolError(fmt.Sprintf("Failed to resolve symbol %q: %v", entityStr, err)), nil
			}

			fqn := dsc.GetFullyQualifiedName()
			var elementType string

			// Determine the type of the descriptor
			switch d := dsc.(type) {
			case *desc.MessageDescriptor:
				elementType = "a message"
				if parent, ok := d.GetParent().(*desc.MessageDescriptor); ok {
					if d.IsMapEntry() {
						for _, f := range parent.GetFields() {
							if f.IsMap() && f.GetMessageType() == d {
								elementType = "the entry type for a map field"
								dsc = f
								break
							}
						}
					} else {
						for _, f := range parent.GetFields() {
							if f.GetType() == descriptorpb.FieldDescriptorProto_TYPE_GROUP && f.GetMessageType() == d {
								elementType = "the type of a group field"
								dsc = f
								break
							}
						}
					}
				}
			case *desc.FieldDescriptor:
				elementType = "a field"
				if d.GetType() == descriptorpb.FieldDescriptorProto_TYPE_GROUP {
					elementType = "a group field"
				} else if d.IsExtension() {
					elementType = "an extension"
				}
			case *desc.OneOfDescriptor:
				elementType = "a one-of"
			case *desc.EnumDescriptor:
				elementType = "an enum"
			case *desc.EnumValueDescriptor:
				elementType = "an enum value"
			case *desc.ServiceDescriptor:
				elementType = "a service"
			case *desc.MethodDescriptor:
				elementType = "a method"
			default:
				return toolError(fmt.Sprintf("descriptor has unrecognized type %T", dsc)), nil
			}

			// Get the descriptor text
			txt, err := grpcurl.GetDescriptorText(dsc, descSource)
			if err != nil {
				return toolError(fmt.Sprintf("Failed to describe symbol %q: %v", entityStr, err)), nil
			}

			description := fmt.Sprintf("%s is %s:\n%s", fqn, elementType, txt)

			// // For message types, also show a JSON template
			// if msgDesc, ok := dsc.(*desc.MessageDescriptor); ok {
			// 	tmpl := grpcurl.MakeTemplate(msgDesc)
			// 	options := grpcurl.FormatOptions{EmitJSONDefaultFields: true}
			// 	_, formatter, err := grpcurl.RequestParserAndFormatter(grpcurl.FormatJSON, descSource, nil, options)
			// 	if err != nil {
			// 		return toolError(fmt.Sprintf("Failed to create formatter: %v", err)), nil
			// 	}
			// 	str, err := formatter(tmpl)
			// 	if err != nil {
			// 		return toolError(fmt.Sprintf("Failed to print template for message %s: %v", entityStr, err)), nil
			// 	}
			// 	description += "\nMessage template:\n" + str
			// }

			results = append(results, description)
		}

		return toolSuccess(strings.Join(results, "\n\n")), nil
	})

	return
}

// metadataToMap converts gRPC metadata to a map suitable for JSON marshaling
func metadataToMap(md metadata.MD) map[string]interface{} {
	result := make(map[string]interface{})
	for key, values := range md {
		// If there's only one value, store it directly rather than as an array
		if len(values) == 1 {
			result[key] = values[0]
		} else if len(values) > 0 {
			result[key] = values
		}
	}
	return result
}

// singleMessageSupplier implements grpcurl.RequestSupplier interface for a single message.
type singleMessageSupplier struct {
	data []byte
	used bool
}

// Supply implements the grpcurl.RequestSupplier interface.
func (s *singleMessageSupplier) Supply(msg proto.Message) error {
	if s.used {
		return io.EOF
	}
	s.used = true
	return jsonpb.Unmarshal(bytes.NewReader(s.data), msg)
}

func main() {
	address := os.Getenv("ADDRESS")
	if address == "" {
		log.Fatal("ADDRESS environment variable is required")
		os.Exit(1)
	}
	grpcServer := NewGrpcReflectionServer(address)
	if err := grpcServer.Serve(); err != nil && err != io.EOF {
		log.Fatal("Error serving MCP server:", err)
		os.Exit(1)
	}
}

// WithStringArray adds a string array property to the tool schema.
func WithStringArray(name string, opts ...mcp.PropertyOption) mcp.ToolOption {
	return func(t *mcp.Tool) {
		schema := map[string]interface{}{
			"type": "array",
			"items": map[string]interface{}{
				"type": "string",
			},
		}

		for _, opt := range opts {
			opt(schema)
		}

		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			if t.InputSchema.Required == nil {
				t.InputSchema.Required = []string{name}
			} else {
				t.InputSchema.Required = append(t.InputSchema.Required, name)
			}
		}

		t.InputSchema.Properties[name] = schema
	}
}

// toolSuccess creates a successful MCP response with the provided text contents.
func toolSuccess(contents ...string) *mcp.CallToolResult {
	var iface []interface{}
	for _, c := range contents {
		iface = append(iface, mcp.NewTextContent(c))
	}
	return &mcp.CallToolResult{
		Content: iface,
		IsError: false,
	}
}

// toolError creates an MCP error response with the given error message.
func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []interface{}{mcp.NewTextContent(message)},
		IsError: true,
	}
}

// GrpcReflectionServer wraps grpcurl functionalities into an MCP server.
type GrpcReflectionServer struct {
	srv     *server.MCPServer
	host    string
	headers map[string]string // Global headers to be used with all requests
}

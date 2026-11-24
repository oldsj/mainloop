import Foundation
import HTTPTypes
import OpenAPIRuntime
import OpenAPIURLSession

/// Builds the generated API client for one Mainloop origin.
///
/// The production web app calls the API on its own origin under `/api`, and so does this app.
/// Every request carries `X-Mainloop-Client: ios`: owner writes require the header
/// (docs/specs/authentication.md, "Owner writes"), and with it the gateway answers a lapsed
/// sign-in with 401 instead of a login redirect.
public enum MainloopAPI {
    public static let clientHeaderValue = "ios"

    public static func client(
        origin: URL,
        middlewares: [any ClientMiddleware] = [],
        transport: any ClientTransport = URLSessionTransport()
    ) -> Client {
        Client(
            serverURL: apiBaseURL(origin: origin),
            transport: transport,
            middlewares: [ClientHeaderMiddleware()] + middlewares
        )
    }

    /// `https://host` becomes `https://host/api`. A path or query on the origin is dropped: the
    /// app talks to exactly one origin.
    public static func apiBaseURL(origin: URL) -> URL {
        var parts = URLComponents()
        parts.scheme = origin.scheme
        parts.host = origin.host()
        parts.port = origin.port
        parts.path = "/api"
        return parts.url!
    }
}

/// Adds `X-Mainloop-Client` to every request.
public struct ClientHeaderMiddleware: ClientMiddleware {
    public static let name = HTTPField.Name("X-Mainloop-Client")!

    public init() {}

    public func intercept(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String,
        next: @Sendable (HTTPRequest, HTTPBody?, URL) async throws -> (HTTPResponse, HTTPBody?)
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        request.headerFields[Self.name] = MainloopAPI.clientHeaderValue
        return try await next(request, body, baseURL)
    }
}

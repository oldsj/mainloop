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

    /// `https://host` becomes `https://host/api`. A path, query or user on the origin is
    /// dropped: the app talks to exactly one origin. The host is kept as written, so an IPv6
    /// literal keeps its brackets.
    public static func apiBaseURL(origin: URL) -> URL {
        guard var parts = URLComponents(url: origin, resolvingAgainstBaseURL: false) else {
            return origin.appending(path: "api")
        }
        parts.user = nil
        parts.password = nil
        parts.query = nil
        parts.fragment = nil
        parts.path = "/api"
        return parts.url ?? origin.appending(path: "api")
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

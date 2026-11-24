import CryptoKit
import Foundation
import MainloopAPI

/// The app's own Pocket ID sign-in: the authorization-code flow with PKCE against a public OIDC
/// client (no secret), used once to register the phone (docs/specs/devices.md). The browser part
/// (ASWebAuthenticationSession) is the app's; this builds the request and checks the answer.
public struct PocketIDSignIn: Sendable {
    public static let redirectURI = "mainloop-ios://oauth/callback"
    public static let callbackScheme = "mainloop-ios"
    public static let scopes = "openid profile email groups"

    public let issuer: URL
    public let clientID: String

    public init(issuer: URL, clientID: String) {
        self.issuer = issuer
        self.clientID = clientID
    }

    /// From the app's build settings: an HTTPS issuer and a client id, or nil (no sign-in).
    public static func fromSettings(issuer: String?, clientID: String?) -> PocketIDSignIn? {
        guard let issuer = issuer?.trimmingCharacters(in: .whitespaces), let clientID = clientID?.trimmingCharacters(in: .whitespaces),
              !clientID.isEmpty, let url = URL(string: issuer), url.scheme == "https", url.host() != nil,
              url.query == nil, url.fragment == nil
        else { return nil }
        return PocketIDSignIn(issuer: url, clientID: clientID)
    }

    /// One sign-in attempt's secrets, kept in memory until the callback.
    public struct Attempt: Sendable, Equatable {
        public let verifier: String
        public let state: String
        public let nonce: String

        public init(verifier: String = PKCE.verifier(), state: String = PKCE.verifier(), nonce: String = PKCE.verifier()) {
            self.verifier = verifier
            self.state = state
            self.nonce = nonce
        }
    }

    public struct Endpoints: Sendable, Equatable, Decodable {
        public let issuer: String
        public let authorizationEndpoint: URL
        public let tokenEndpoint: URL

        enum CodingKeys: String, CodingKey {
            case issuer
            case authorizationEndpoint = "authorization_endpoint"
            case tokenEndpoint = "token_endpoint"
        }
    }

    public enum Failure: Error, Equatable {
        case discovery(String)
        case callback(String)
        case token(String)
    }

    /// Reads the issuer's discovery document, which must name the same issuer and HTTPS endpoints.
    public func discover(using session: URLSession = .shared) async throws -> Endpoints {
        let url = issuer.appending(path: ".well-known/openid-configuration")
        let (data, response) = try await session.data(from: url)
        guard (response as? HTTPURLResponse)?.statusCode == 200 else { throw Failure.discovery("status") }
        let endpoints = try JSONDecoder().decode(Endpoints.self, from: data)
        guard endpoints.issuer == issuer.absoluteString.trimmingSuffix("/"),
              endpoints.authorizationEndpoint.scheme == "https", endpoints.tokenEndpoint.scheme == "https"
        else { throw Failure.discovery("the document names another issuer or a non-HTTPS endpoint") }
        return endpoints
    }

    /// The oldest sign-in, in seconds, that may register a device: the server's limit.
    public static let maxAge = 600

    public func authorizationURL(_ endpoints: Endpoints, _ attempt: Attempt) -> URL {
        var parts = URLComponents(url: endpoints.authorizationEndpoint, resolvingAgainstBaseURL: false)!
        parts.queryItems = (parts.queryItems ?? []) + [
            .init(name: "response_type", value: "code"),
            .init(name: "client_id", value: clientID),
            .init(name: "redirect_uri", value: Self.redirectURI),
            .init(name: "scope", value: Self.scopes),
            .init(name: "state", value: attempt.state),
            .init(name: "nonce", value: attempt.nonce),
            .init(name: "code_challenge", value: PKCE.challenge(for: attempt.verifier)),
            .init(name: "code_challenge_method", value: "S256"),
            // Always ask: a registration must come from a sign-in the owner just made.
            .init(name: "prompt", value: "login"),
            // max_age makes the ID token carry auth_time (OIDC Core 3.1.2.1), which the server
            // requires within the same 10 minutes.
            .init(name: "max_age", value: String(Self.maxAge)),
        ]
        return parts.url!
    }

    /// The authorization code from the callback, after checking it belongs to this attempt.
    public func code(from callback: URL, _ attempt: Attempt) throws -> String {
        guard callback.absoluteString.hasPrefix(Self.redirectURI),
              let items = URLComponents(url: callback, resolvingAgainstBaseURL: false)?.queryItems
        else { throw Failure.callback("not this app's callback") }
        func value(_ name: String) -> String? { items.first { $0.name == name }?.value }
        if let error = value("error") { throw Failure.callback(error) }
        guard value("state") == attempt.state else { throw Failure.callback("state does not match") }
        guard let code = value("code"), !code.isEmpty else { throw Failure.callback("no code") }
        return code
    }

    /// Exchanges the code for an ID token and checks its nonce. The server verifies the token's
    /// signature, issuer, audience, subject and age.
    public func exchange(code: String, _ endpoints: Endpoints, _ attempt: Attempt, using session: URLSession = .shared) async throws -> String {
        var request = URLRequest(url: endpoints.tokenEndpoint)
        request.httpMethod = "POST"
        request.setValue("application/x-www-form-urlencoded", forHTTPHeaderField: "Content-Type")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.httpBody = Self.form([
            "grant_type": "authorization_code", "code": code, "redirect_uri": Self.redirectURI,
            "client_id": clientID, "code_verifier": attempt.verifier,
        ])
        let (data, response) = try await session.data(for: request)
        guard (response as? HTTPURLResponse)?.statusCode == 200 else { throw Failure.token("status") }
        struct TokenResponse: Decodable { let id_token: String }
        let idToken = try JSONDecoder().decode(TokenResponse.self, from: data).id_token
        guard Self.claim("nonce", in: idToken) == attempt.nonce else { throw Failure.token("nonce does not match") }
        return idToken
    }

    /// A string claim from a JWT's payload, unverified.
    public static func claim(_ name: String, in jwt: String) -> String? {
        let parts = jwt.split(separator: ".")
        guard parts.count == 3, let payload = Base64URL.decode(String(parts[1])),
              let object = try? JSONSerialization.jsonObject(with: payload) as? [String: Any]
        else { return nil }
        return object[name] as? String
    }

    static func form(_ fields: [String: String]) -> Data {
        var allowed = CharacterSet.alphanumerics
        allowed.insert(charactersIn: "-._~")
        return Data(fields.sorted { $0.key < $1.key }
            .map { "\($0.key)=\($0.value.addingPercentEncoding(withAllowedCharacters: allowed) ?? "")" }
            .joined(separator: "&").utf8)
    }
}

/// PKCE (RFC 7636) with S256.
public enum PKCE {
    /// 43 characters from 32 random bytes.
    public static func verifier() -> String {
        var bytes = [UInt8](repeating: 0, count: 32)
        for i in bytes.indices { bytes[i] = UInt8.random(in: .min ... .max) }
        return Base64URL.encode(bytes)
    }

    public static func challenge(for verifier: String) -> String {
        Base64URL.encode(Data(SHA256.hash(data: Data(verifier.utf8))))
    }
}

extension String {
    func trimmingSuffix(_ suffix: String) -> String {
        hasSuffix(suffix) ? String(dropLast(suffix.count)) : self
    }
}

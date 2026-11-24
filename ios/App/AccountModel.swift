import AuthenticationServices
import MainloopAPI
import MainloopCore
import SwiftUI
import UIKit

/// This phone's registration, as Settings shows it (docs/specs/devices.md).
@MainActor @Observable
final class AccountModel {
    enum Phase: Equatable {
        case unavailable
        case signedOut
        case working
        case signedIn(deviceID: String)
    }

    private(set) var phase: Phase
    var lastError: String?

    let account: DeviceAccount
    private let origin: URL?
    private let signIn: PocketIDSignIn?

    init(account: DeviceAccount, origin: URL?, signIn: PocketIDSignIn?) {
        self.account = account
        self.origin = origin
        self.signIn = signIn
        if let credential = account.credential, credential.origin == origin {
            phase = .signedIn(deviceID: credential.deviceID)
        } else {
            phase = origin != nil && signIn != nil ? .signedOut : .unavailable
        }
    }

    /// Makes a new request key, signs in with Pocket ID in a private browser session for that key
    /// (the nonce is its thumbprint) and registers it. The Pocket ID token is used once and never
    /// stored.
    func register(using session: WebAuthenticationSession) async {
        guard let origin, let signIn, phase == .signedOut else { return }
        phase = .working
        lastError = nil
        do {
            #if targetEnvironment(simulator)
                let key = try NewDeviceKey.make(allowSoftware: true)
            #else
                let key = try NewDeviceKey.make(allowSoftware: false)
            #endif
            let endpoints = try await signIn.discover()
            let attempt = PocketIDSignIn.Attempt(nonce: key.signer.thumbprint)
            let callback = try await session.authenticate(
                using: signIn.authorizationURL(endpoints, attempt),
                callback: .customScheme(PocketIDSignIn.callbackScheme),
                preferredBrowserSession: .ephemeral,
                additionalHeaderFields: [:]
            )
            let code = try signIn.code(from: callback, attempt)
            let idToken = try await signIn.exchange(code: code, endpoints, attempt)
            let credential = try await DeviceEnrollment.register(
                origin: origin, idToken: idToken, name: UIDevice.current.name, key: key
            )
            do {
                try account.save(credential)
            } catch {
                // Registered, but the phone can't keep it: revoke it while the key is in hand.
                await DeviceEnrollment.revoke(credential)
                throw error
            }
            phase = .signedIn(deviceID: credential.deviceID)
        } catch let error as ASWebAuthenticationSessionError where error.code == .canceledLogin {
            phase = .signedOut
        } catch {
            lastError = Self.describe(error)
            phase = .signedOut
        }
    }

    func signOut() async {
        phase = .working
        try? await account.signOut()
        phase = origin != nil && signIn != nil ? .signedOut : .unavailable
    }

    private static func describe(_ error: Error) -> String {
        switch error {
        case DeviceError.registrationRefused(403): "Pocket ID signed you in, but Mainloop refused to register this phone."
        case DeviceError.registrationRefused(let status): "Mainloop refused to register this phone (\(status))."
        case DeviceError.noSecureEnclave: "This phone has no Secure Enclave."
        case DeviceError.alreadyRegistered: "This key is already registered. Try again."
        case is PocketIDSignIn.Failure: "Pocket ID sign-in failed."
        default: "Couldn't register this phone: \(error.localizedDescription)"
        }
    }
}

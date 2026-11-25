import AuthenticationServices
import MainloopAPI
import MainloopCore
import SwiftUI
import UIKit

/// This phone's pairing (docs/specs/devices.md): its request key and approval key, registered
/// with Mainloop through a Pocket ID sign-in.
@MainActor @Observable
final class AccountModel {
    enum Phase: Equatable {
        case unavailable
        case signedOut
        case working
        case signedIn(deviceID: String)
    }

    private(set) var phase: Phase
    /// Why the last sign-in failed, in a sentence that says what to do.
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

    /// Makes a new request key and approval key, signs in with Pocket ID in a private browser
    /// session for that key (the nonce is its thumbprint) and registers it. The Pocket ID token is
    /// used once and never stored. True when this iPhone is paired.
    func register(using session: WebAuthenticationSession) async -> Bool {
        guard let origin, let signIn, phase == .signedOut else { return false }
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
            return true
        } catch let error as ASWebAuthenticationSessionError where error.code == .canceledLogin {
            phase = .signedOut
        } catch {
            lastError = Self.describe(error, origin: origin)
            phase = .signedOut
        }
        return false
    }

    /// The request key: where it lives.
    var requestKey: String? {
        guard case .signedIn = phase, let credential = account.credential else { return nil }
        return credential.keyKind == .secureEnclave ? "Secure Enclave" : "Software key (simulator)"
    }

    /// How this phone approves privileged actions; nil while signed out.
    var approvals: String? {
        guard case .signedIn = phase, let credential = account.credential, credential.approvalKeyData != nil else { return nil }
        return credential.keyKind == .secureEnclave ? "Secure Enclave, Touch ID each time" : "Software key (simulator), no Touch ID"
    }

    func signOut() async {
        phase = .working
        try? await account.signOut()
        phase = origin != nil && signIn != nil ? .signedOut : .unavailable
    }

    private static func describe(_ error: Error, origin: URL) -> String {
        let host = origin.host() ?? origin.absoluteString
        switch error {
        case DeviceError.registrationRefused(403):
            return "Pocket ID signed you in, but Mainloop refused to pair this iPhone. Mainloop pairs only its owner's Pocket ID account, through the iPhone app's own client (MAINLOOP_AUTH_DEVICE_CLIENT_ID)."
        case DeviceError.registrationRefused(let status):
            return "Mainloop refused to pair this iPhone (it answered \(status)). Check that \(host) runs a version with device pairing."
        case DeviceError.noSecureEnclave:
            return "This iPhone has no Secure Enclave, which pairing needs for its keys."
        case DeviceError.alreadyRegistered:
            return "That key was already paired. Try again: a new key is made each time."
        case is PocketIDSignIn.Failure:
            return "Pocket ID didn't complete the sign-in. Try again; if it keeps failing, check the iPhone app's client in Pocket ID."
        case is URLError:
            return "Couldn't reach Pocket ID or \(host). They run on your home network: connect to it or to its VPN, then try again."
        default:
            return "Couldn't pair this iPhone. Try again."
        }
    }
}

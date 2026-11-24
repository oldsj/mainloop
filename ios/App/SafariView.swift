import SafariServices
import SwiftUI

/// The web app for screens that aren't native yet. SFSafariViewController keeps its own sign-in
/// and supports passkeys on the identity provider, which an in-app web view would not.
struct SafariView: UIViewControllerRepresentable {
    let url: URL

    func makeUIViewController(context: Context) -> SFSafariViewController {
        let configuration = SFSafariViewController.Configuration()
        configuration.entersReaderIfAvailable = false
        return SFSafariViewController(url: url, configuration: configuration)
    }

    func updateUIViewController(_ controller: SFSafariViewController, context: Context) {}
}

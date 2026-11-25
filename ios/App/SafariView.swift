import SafariServices
import SwiftUI

/// A page that isn't Mainloop's (a pull request, docs), opened from the web app in a Safari sheet
/// with Safari's own cookies, never in the app's web view.
struct SafariView: UIViewControllerRepresentable {
    let url: URL

    func makeUIViewController(context: Context) -> SFSafariViewController {
        let configuration = SFSafariViewController.Configuration()
        configuration.entersReaderIfAvailable = false
        return SFSafariViewController(url: url, configuration: configuration)
    }

    func updateUIViewController(_ controller: SFSafariViewController, context: Context) {}
}

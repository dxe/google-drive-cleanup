import type { Metadata } from 'next';
import './globals.css';

export const metadata: Metadata = {
  title: 'Drive cleanup review',
  description: 'Mark crawled Google Drive files and folders as keep or delete',
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}

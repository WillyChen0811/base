# Copyright: (c) 2019, Jordan Borean (@jborean93) <jborean93@gmail.com>
# MIT License (see LICENSE or https://opensource.org/licenses/MIT)

import warnings

from pybase._vendor.smbprotocol.file_info import *

warnings.warn(
    "The smbprotocol.query_info file has been renamed to smbprotocol.file_info and will be removed in the "
    "next major release.",
    DeprecationWarning,
)
